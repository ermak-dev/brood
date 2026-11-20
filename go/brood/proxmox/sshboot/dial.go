package sshboot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// attemptTimeout bounds one attempt: the TCP connect and, separately,
	// the SSH handshake.
	attemptTimeout = 10 * time.Second
	// retryWindow is how long Dial waits for sshd of a just-started
	// container.
	retryWindow = 2 * time.Minute
	// retryPause separates attempts.
	retryPause = 2 * time.Second
)

// ErrHostKey reports that the container presented a host key other than the
// pinned one.
var ErrHostKey = errors.New("sshboot: host key does not match the pinned key")

// Target is one container to reach.
type Target struct {
	// Addr is the container address and SSH port (22).
	Addr netip.AddrPort
	// HostKey is the pinned host public key in authorized_keys form; empty
	// means trust on first use.
	HostKey string
}

// Session is an open SSH connection as root.
type Session struct {
	client  *ssh.Client
	hostKey string
	once    sync.Once
	err     error
}

// Dial connects as user root with public key authentication only. It retries
// connection refusals, unreachable hosts, timeouts and connections dropped
// during the handshake until ctx is done or two minutes elapse, because sshd
// of a just-started container needs a few seconds. Authentication failures
// and host key mismatches are not retried. With an empty Target.HostKey the
// first presented host key is accepted and returned by Session.HostKey so the
// caller can pin it; with a non-empty one any other key is refused with
// ErrHostKey.
func Dial(ctx context.Context, id *Identity, t Target) (*Session, error) {
	if id == nil {
		return nil, errors.New("sshboot: dial without an identity")
	}
	if !t.Addr.IsValid() || t.Addr.Port() == 0 {
		return nil, errors.New("sshboot: dial without a target address")
	}
	var pinned ssh.PublicKey
	if t.HostKey != "" {
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.HostKey))
		if err != nil {
			return nil, fmt.Errorf("sshboot: pinned host key of %s is malformed: %w", t.Addr, err)
		}
		pinned = key
	}
	deadline := time.Now().Add(retryWindow)
	for {
		s, err := dialOnce(ctx, id, t.Addr, pinned)
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryable(err) || time.Now().Add(retryPause).After(deadline) {
			return nil, fmt.Errorf("sshboot: dial %s: %w", t.Addr, err)
		}
		pause := time.NewTimer(retryPause)
		select {
		case <-pause.C:
		case <-ctx.Done():
			pause.Stop()
			return nil, ctx.Err()
		}
	}
}

// dialOnce makes one connection attempt.
func dialOnce(ctx context.Context, id *Identity, addr netip.AddrPort, pinned ssh.PublicKey) (*Session, error) {
	var presented ssh.PublicKey
	mismatch := false
	config := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{ssh.PublicKeys(id.signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if pinned != nil && !bytes.Equal(key.Marshal(), pinned.Marshal()) {
				mismatch = true
				return ErrHostKey
			}
			presented = key
			return nil
		},
		Timeout: attemptTimeout,
	}
	if pinned != nil {
		// sshd has host keys of several types and picks one by the client's
		// preference; without this a pinned ed25519 key could be compared
		// with the server's ECDSA key.
		config.HostKeyAlgorithms = hostKeyAlgorithms(pinned.Type())
	}
	dialer := net.Dialer{Timeout: attemptTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, err
	}
	// NewClientConn knows neither ctx nor ClientConfig.Timeout: the deadline
	// bounds the handshake, closing the connection interrupts it.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(attemptTimeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr.String(), config)
	if err == nil {
		err = conn.SetDeadline(time.Time{})
		if err != nil {
			c.Close()
		}
	}
	if !stop() && err == nil {
		// ctx ended right after the handshake: the connection is closed.
		c.Close()
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		var negotiation *ssh.AlgorithmNegotiationError
		if mismatch || (pinned != nil && errors.As(err, &negotiation) && negotiation.What == "host key") {
			// The second case: the server has no key of the pinned type.
			return nil, ErrHostKey
		}
		return nil, err
	}
	return &Session{
		client:  ssh.NewClient(c, chans, reqs),
		hostKey: authorizedKey(presented),
	}, nil
}

// hostKeyAlgorithms lists the signature algorithms of one host key type.
func hostKeyAlgorithms(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

// retryable tells a container whose sshd is not up yet from a refusal that
// another attempt will not change.
func retryable(err error) bool {
	if errors.Is(err, ErrHostKey) {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	// sshd that is starting or restarting drops the connection mid-handshake.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// HostKey returns the host public key the container presented, in
// authorized_keys form without a comment.
func (s *Session) HostKey() string {
	return s.hostKey
}

// Close closes the connection. Commands still running in Run are
// interrupted. Close may be called more than once.
func (s *Session) Close() error {
	s.once.Do(func() { s.err = s.client.Close() })
	return s.err
}
