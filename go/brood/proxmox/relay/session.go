package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"golang.org/x/crypto/ssh"
)

const sessionChannel = "brood-session-rpc-v1"

// MaxSessionBytes bounds each encoded session request and response.
const MaxSessionBytes = 128 * 1024

// WorkspaceAttestOperation asks the guest root to stop every actor process,
// bring the workspace to a commit and sign the result (#183).
const WorkspaceAttestOperation = "workspace_attest"

// GuestReleaseOperation asks the guest root which guest release (#184) its
// /opt/brood/bin names (#133). The guest reads only its own artifact root and
// answers a GuestRelease as JSON in the response text; a guest runtime that
// predates the operation answers unsupported.
const GuestReleaseOperation = "guest_release"

// GuestRelease answers GuestReleaseOperation.
type GuestRelease struct {
	// Layout is "release" when bin links to releases/<id> and "plain" when bin
	// is the plain directory of a bootstrap before #184.
	Layout string `json:"layout"`
	// Release is the <id> that bin links to; empty for a plain bin.
	Release string `json:"release,omitempty"`
}

// sessionTimeout bounds one session call on both peers. Attestation fetches
// and hashes a snapshot, which does not fit the interactive 20 s budget.
func sessionTimeout(operation string) time.Duration {
	if operation == WorkspaceAttestOperation {
		return 10 * time.Minute
	}
	return 20 * time.Second
}

// SessionRequest carries a server-authorized operation. Caller is set by the
// authenticated server, never copied from guest-provided arguments.
type SessionRequest struct {
	Operation string          `json:"operation"`
	Arguments json.RawMessage `json:"arguments"`
	Caller    string          `json:"caller"`
}

// SessionResponse distinguishes completed reads from unconfirmed input writes.
type SessionResponse struct {
	OK      bool   `json:"ok"`
	Text    string `json:"text"`
	Outcome string `json:"outcome"`
}

// ReadSessionRequest parses a single bounded frame, rejecting extra fields.
func ReadSessionRequest(r io.Reader) (SessionRequest, error) {
	var req SessionRequest
	data, err := io.ReadAll(io.LimitReader(r, MaxSessionBytes+1))
	if err != nil || len(data) > MaxSessionBytes {
		return req, errors.New("session request exceeds size limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF || req.Operation == "" || len(req.Operation) > 64 || req.Caller == "" || len(req.Caller) > 512 || !json.Valid(req.Arguments) {
		return SessionRequest{}, errors.New("invalid session request")
	}
	return req, nil
}

func sessionResponseBytes(resp SessionResponse) []byte {
	if resp.Outcome == "" {
		resp = SessionResponse{Text: "session operation returned no outcome", Outcome: "failed"}
	}
	data, err := json.Marshal(resp)
	if err != nil || len(data)+1 > MaxSessionBytes {
		data, _ = json.Marshal(SessionResponse{Text: "session response exceeds size limit", Outcome: "failed"})
	}
	return append(data, '\n')
}

func readSessionResponse(r io.Reader) (SessionResponse, error) {
	var resp SessionResponse
	data, err := io.ReadAll(io.LimitReader(r, MaxSessionBytes+1))
	if err != nil || len(data) > MaxSessionBytes {
		return resp, errors.New("invalid session response size")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&resp) != nil || d.Decode(new(any)) != io.EOF || resp.Outcome == "" {
		return SessionResponse{}, errors.New("invalid session response")
	}
	return resp, nil
}

func serveGuestSession(parent context.Context, handler func(context.Context, SessionRequest) SessionResponse, ch ssh.NewChannel) {
	req, err := ReadSessionRequest(bytes.NewReader(ch.ExtraData()))
	if err != nil {
		ch.Reject(ssh.Prohibited, "invalid session request")
		return
	}
	ctx, cancel := context.WithTimeout(parent, sessionTimeout(req.Operation))
	defer cancel()
	stream, requests, err := ch.Accept()
	if err != nil {
		return
	}
	defer stream.Close()
	go ssh.DiscardRequests(requests)
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	go func() { _, _ = io.Copy(io.Discard, io.LimitReader(stream, 1)); cancel() }()
	_, _ = stream.Write(sessionResponseBytes(handler(ctx, req)))
}

// CallSession calls the current guest over an independent bounded SSH channel.
// Authorization and incarnation are checked before dispatch and after reading.
func (h *Hub) CallSession(parent context.Context, uid string, generation int64, req SessionRequest) (SessionResponse, error) {
	data, err := json.Marshal(req)
	if err != nil || len(data) > MaxSessionBytes {
		return SessionResponse{}, errors.New("session request exceeds size limit")
	}
	if _, err := ReadSessionRequest(bytes.NewReader(data)); err != nil {
		return SessionResponse{}, err
	}
	ctx, cancel := context.WithTimeout(parent, sessionTimeout(req.Operation))
	defer cancel()
	if err := ctx.Err(); err != nil {
		return SessionResponse{}, err
	}
	p := h.session(uid, generation)
	if p == nil {
		return SessionResponse{}, errUnavailable
	}
	valid := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return !h.closed && h.peers[p.id] == p && h.authorize(uid, generation, p.token)
	}
	h.mu.Lock()
	if h.closed || h.peers[p.id] != p || p.streams >= MaxStreams || !h.authorize(uid, generation, p.token) {
		h.mu.Unlock()
		return SessionResponse{}, errUnavailable
	}
	p.streams++
	h.mu.Unlock()
	release := func() { h.mu.Lock(); p.streams--; h.mu.Unlock() }
	type opened struct {
		ch       ssh.Channel
		requests <-chan *ssh.Request
		err      error
	}
	result := make(chan opened)
	openCtx, openCancel := context.WithTimeout(ctx, handshakeTimeout)
	defer openCancel()
	go func() {
		var ch ssh.Channel
		var requests <-chan *ssh.Request
		unlock, err := openSessionGuard(openCtx)
		if err == nil {
			// У SSH нет отмены channel-open. Только на время открытия
			// отмена закрывает peer, чтобы зависший гость не держал gate
			// бесконечно. Ожидание результата не разрывает общий relay.
			closed := make(chan struct{})
			stopOpening := context.AfterFunc(openCtx, func() {
				p.conn.Close()
				close(closed)
			})
			ch, requests, err = p.conn.OpenChannel(sessionChannel, data)
			if !stopOpening() {
				<-closed
			}
			unlock()
		}
		select {
		case result <- opened{ch, requests, err}:
		case <-openCtx.Done():
			if ch != nil {
				ch.Close()
			}
			release()
		}
	}()
	select {
	case <-openCtx.Done():
		return SessionResponse{}, openCtx.Err()
	case r := <-result:
		defer release()
		if r.err != nil {
			return SessionResponse{}, errUnavailable
		}
		defer r.ch.Close()
		go ssh.DiscardRequests(r.requests)
		if !valid() {
			return SessionResponse{}, errUnavailable
		}
		stop := context.AfterFunc(ctx, func() { r.ch.Close() })
		defer stop()
		resp, err := readSessionResponse(r.ch)
		if ctx.Err() != nil {
			return SessionResponse{}, ctx.Err()
		}
		if !valid() {
			return SessionResponse{}, errUnavailable
		}
		return resp, err
	}
}
