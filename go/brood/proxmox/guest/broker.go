//go:build linux

package guest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"golang.org/x/sys/unix"
)

const swarmSocket = "/run/brood/swarm.sock"

type brokerSession struct {
	ctx  context.Context
	call relay.SwarmCaller
}

// SwarmBroker exposes bounded swarm operations to UID 1000 without sharing
// machine credentials. Only a currently authenticated relay session can serve.
type SwarmBroker struct {
	listener *net.UnixListener
	lock     *os.File
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	session  *brokerSession
}

// StartSwarmBroker binds a root-owned socket under an exclusive supervisor
// lock. Only a provably stale owned socket can be recovered after a crash.
func StartSwarmBroker(ctx context.Context) (*SwarmBroker, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("swarm broker requires guest supervisor")
	}
	if err := os.Mkdir("/run/brood", 0755); err != nil && !os.IsExist(err) {
		return nil, errors.New("swarm broker directory unavailable")
	}
	info, err := os.Lstat("/run/brood")
	if err != nil {
		return nil, errors.New("swarm broker directory unavailable")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || st.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("swarm broker directory is not trusted")
	}
	listener, lock, err := listenSwarmSocket(swarmSocket, 0)
	if err != nil {
		return nil, errors.New("swarm broker socket unavailable")
	}
	if os.Chown(swarmSocket, 0, 1000) != nil || os.Chmod(swarmSocket, 0660) != nil {
		listener.Close()
		lock.Close()
		return nil, errors.New("swarm broker permissions failed")
	}
	return newSwarmBroker(ctx, listener, lock), nil
}

func newSwarmBroker(parent context.Context, listener *net.UnixListener, lock *os.File) *SwarmBroker {
	ctx, cancel := context.WithCancel(parent)
	b := &SwarmBroker{listener: listener, lock: lock, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go b.serve()
	return b
}

func (b *SwarmBroker) bind(ctx context.Context, call relay.SwarmCaller) func() {
	session := &brokerSession{ctx: ctx, call: call}
	b.mu.Lock()
	b.session = session
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.session == session {
			b.session = nil
		}
	}
}

// ServeSession installs only this connection's caller and clears it on loss.
func (b *SwarmBroker) ServeSession(ctx context.Context, call relay.SwarmCaller) {
	release := b.bind(ctx, call)
	defer release()
	<-ctx.Done()
}

func (b *SwarmBroker) serve() {
	defer close(b.done)
	if b.lock != nil {
		defer b.lock.Close()
	}
	stop := context.AfterFunc(b.ctx, func() { b.listener.Close() })
	defer stop()
	slots := make(chan struct{}, relay.MaxMessagesInFlight)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		c, err := b.listener.AcceptUnix()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		workers.Add(1)
		go func() { defer workers.Done(); defer func() { <-slots }(); defer c.Close(); b.serveConn(c) }()
	}
}

func (b *SwarmBroker) serveConn(c *net.UnixConn) {
	raw, err := c.SyscallConn()
	if err != nil {
		return
	}
	allowed := false
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		allowed = err == nil && cred.Uid == 1000
	}); err != nil || !allowed {
		return
	}
	stop := context.AfterFunc(b.ctx, func() { c.Close() })
	defer stop()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	data, err := bufio.NewReader(io.LimitReader(c, relay.MaxMessageBytes+1)).ReadBytes('\n')
	req, valid := relay.DecodeSwarmRequest(data)
	response := relay.SwarmResponse{Text: "swarm request refused or relay is disconnected"}
	if err == nil && valid {
		b.mu.Lock()
		session := b.session
		b.mu.Unlock()
		if session != nil && session.ctx.Err() == nil {
			ctx, cancel := context.WithTimeout(b.ctx, time.Duration(req.WaitSeconds)*time.Second+15*time.Second)
			response = session.call(ctx, req)
			cancel()
		}
	}
	_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	encoded, err := json.Marshal(response)
	if err == nil && len(encoded) <= relay.MaxMessageBytes {
		_, _ = c.Write(append(encoded, '\n'))
	}
}

func (b *SwarmBroker) Close() error { b.cancel(); <-b.done; return nil }
func (b *SwarmBroker) Wait() error  { <-b.done; return nil }

// CallSwarm uses the fixed local broker, never supervisor credentials.
func CallSwarm(ctx context.Context, req relay.SwarmRequest) relay.SwarmResponse {
	return callSwarmBroker(ctx, swarmSocket, req)
}

func callSwarmBroker(ctx context.Context, path string, req relay.SwarmRequest) relay.SwarmResponse {
	failed := relay.SwarmResponse{Text: "swarm broker unavailable"}
	data, err := json.Marshal(req)
	if err != nil || len(data) > relay.MaxMessageBytes {
		return failed
	}
	c, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", path)
	if err != nil {
		return failed
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	deadline := time.Now().Add(time.Duration(req.WaitSeconds)*time.Second + 15*time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	_ = c.SetDeadline(deadline)
	if _, err = c.Write(append(data, '\n')); err != nil {
		return failed
	}
	data, err = bufio.NewReader(io.LimitReader(c, relay.MaxMessageBytes+1)).ReadBytes('\n')
	var response relay.SwarmResponse
	if err != nil || len(data) > relay.MaxMessageBytes || json.Unmarshal(data, &response) != nil || response.Text == "" {
		return failed
	}
	return response
}
