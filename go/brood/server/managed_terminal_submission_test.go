package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/proxy"
	"brood.dev/go/brood/store"
)

type terminalFaultWriter struct {
	http.ResponseWriter
	conn net.Conn
	rw   *bufio.ReadWriter
}

func (w terminalFaultWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.conn, w.rw, nil }

type terminalRejectWriter struct{ calls *atomic.Int32 }

func (w terminalRejectWriter) Write([]byte) (int, error) {
	w.calls.Add(1)
	return 0, errors.New("client cannot receive 101")
}

func terminalSubmissionFixture(t *testing.T) (*Server, store.ManagedActor, string, string) {
	t.Helper()
	ts, master, target := coordinationServerSetup(t)
	ctx := context.Background()
	editor := store.User{ID: store.NewID(), Username: "terminal-editor", PasswordHash: "test-only"}
	if err := ts.srv.store.CreateUser(ctx, editor); err != nil {
		t.Fatal(err)
	}
	sw, err := ts.srv.store.GetSwarm(ctx, target.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.AddMember(ctx, sw.ProjectID, editor.ID); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetSwarmMember(ctx, sw.ID, editor.ID, string(app.RoleEditor)); err != nil {
		t.Fatal(err)
	}
	token, _, err := auth.NewSessions(ts.srv.store, 0).Start(ctx, editor.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ts.srv, target, token, master.DelegatorID
}

func terminalSubmissionHandler(t *testing.T, s *Server, dial func(context.Context, string, string) (net.Conn, error)) http.Handler {
	t.Helper()
	p := proxy.New(proxy.Config{Swarms: s.gateway, CurrentUser: proxyCurrentUser, Authorize: func(string, app.Action, app.Resource) (bool, error) { return true, nil }, Upstream: viewResolver{target: &url.URL{Scheme: "http", Host: "terminal.invalid"}}, DialContext: dial, BeginUpgrade: s.beginManagedTerminal})
	t.Cleanup(p.Close)
	mux := http.NewServeMux()
	p.Mount(mux)
	return s.auth.Middleware(mux)
}

func terminalSubmissionRequest(target store.ManagedActor, token string) *http.Request {
	r := httptest.NewRequest("GET", "http://panel.invalid/swarms/"+target.SwarmID+"/actors/"+target.Name+"/ws?mode=rw", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.AddCookie(&http.Cookie{Name: auth.CookieSession, Value: token})
	return r
}

func terminalSubmissionWriter(t *testing.T, output io.Writer) terminalFaultWriter {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return terminalFaultWriter{httptest.NewRecorder(), a, bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), bufio.NewWriter(output))}
}

func TestManagedTerminalUnknownSubmissionRetainsHold(t *testing.T) {
	for _, fault := range []string{"lost upstream response", "client 101 write", "effect then refusal"} {
		t.Run(fault, func(t *testing.T) {
			s, target, token, _ := terminalSubmissionFixture(t)
			var effects atomic.Int32
			h := terminalSubmissionHandler(t, s, func(context.Context, string, string) (net.Conn, error) {
				a, b := net.Pipe()
				go func() {
					defer b.Close()
					if _, err := http.ReadRequest(bufio.NewReader(b)); err != nil {
						return
					}
					effects.Add(1)
					switch fault {
					case "client 101 write":
						io.WriteString(b, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
					case "effect then refusal":
						io.WriteString(b, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
					}
				}()
				return a, nil
			})
			var clientWrites atomic.Int32
			var output io.Writer = io.Discard
			if fault == "client 101 write" {
				output = terminalRejectWriter{&clientWrites}
			}
			h.ServeHTTP(terminalSubmissionWriter(t, output), terminalSubmissionRequest(target, token))
			if effects.Load() != 1 {
				t.Fatalf("fault did not execute upstream effect: %d", effects.Load())
			}
			if fault == "client 101 write" && clientWrites.Load() != 1 {
				t.Fatal("client 101 failure was not exercised")
			}
			held, err := s.store.CurrentActorOperation(context.Background(), target.UID)
			if err != nil || held.Operation != "terminal_stream" || held.State != "unconfirmed" || held.Owner {
				t.Fatalf("effect lost reservation: %+v %v", held, err)
			}
		})
	}
}

func TestManagedTerminalDefinitePreSubmitFailureSettles(t *testing.T) {
	for _, fault := range []string{"dial", "authority revoked", "request canceled"} {
		t.Run(fault, func(t *testing.T) {
			s, target, token, _ := terminalSubmissionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var effects atomic.Int32
			upstreamDone := make(chan struct{})
			h := terminalSubmissionHandler(t, s, func(context.Context, string, string) (net.Conn, error) {
				if fault == "dial" {
					close(upstreamDone)
					return nil, errors.New("cannot open upstream")
				}
				if fault == "authority revoked" {
					if err := auth.NewSessions(s.store, 0).Revoke(context.Background(), token); err != nil {
						t.Error(err)
					}
				} else {
					cancel()
				}
				a, b := net.Pipe()
				go func() {
					defer close(upstreamDone)
					defer b.Close()
					if _, err := http.ReadRequest(bufio.NewReader(b)); err == nil {
						effects.Add(1)
					}
				}()
				return a, nil
			})
			h.ServeHTTP(terminalSubmissionWriter(t, io.Discard), terminalSubmissionRequest(target, token).WithContext(ctx))
			<-upstreamDone
			if effects.Load() != 0 {
				t.Fatalf("pre-submit failure reached upstream: %d", effects.Load())
			}
			if held, err := s.store.CurrentActorOperation(context.Background(), target.UID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("definite no-submit did not settle: %+v %v", held, err)
			}
		})
	}
}

type terminalSubmissionConn struct {
	net.Conn
	beforeWrite func()
}

func (c terminalSubmissionConn) Write(p []byte) (int, error) {
	c.beforeWrite()
	return c.Conn.Write(p)
}

func TestManagedTerminalGateCoversWriteNotResponseOrStream(t *testing.T) {
	s, target, token, owner := terminalSubmissionFixture(t)
	requestReceived, respond, upstreamDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var unfencedWrite atomic.Bool
	h := terminalSubmissionHandler(t, s, func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			defer close(upstreamDone)
			defer b.Close()
			if _, err := http.ReadRequest(bufio.NewReader(b)); err != nil {
				return
			}
			close(requestReceived)
			<-respond
			io.WriteString(b, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			io.Copy(io.Discard, b)
		}()
		return terminalSubmissionConn{Conn: a, beforeWrite: func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if release, err := s.managed.effects.acquire(ctx, target.UID); err == nil {
				unfencedWrite.Store(true)
				release()
			}
		}}, nil
	})
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	b.SetDeadline(time.Now().Add(5 * time.Second))
	w := terminalFaultWriter{httptest.NewRecorder(), a, bufio.NewReadWriter(bufio.NewReader(a), bufio.NewWriter(a))}
	done := make(chan struct{})
	r := terminalSubmissionRequest(target, token)
	go func() { defer close(done); h.ServeHTTP(w, r) }()
	select {
	case <-requestReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request did not arrive")
	}
	intervene := func() store.ActorOperation {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		release, err := s.managed.effects.acquire(ctx, target.UID)
		if err != nil {
			t.Fatalf("owner blocked by terminal response or stream: %v", err)
		}
		defer release()
		op, err := s.store.AcquireOwnerActorOperation(ctx, target, store.NewID(), "owner:test", "slash", owner, func(context.Context) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	first := intervene()
	close(respond)
	resp, err := http.ReadResponse(bufio.NewReader(b), r)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("stream did not open: %v %v", resp, err)
	}
	select {
	case <-done:
		t.Fatal("stream ended before owner intervention")
	default:
	}
	intervene()
	b.Close()
	<-done
	<-upstreamDone
	if unfencedWrite.Load() {
		t.Fatal("owner gate was available during the upstream request write")
	}
	held, err := s.store.CurrentActorOperation(context.Background(), target.UID)
	if err != nil || held.InterruptedBy != first.OperationID || held.State != "unconfirmed" {
		t.Fatalf("stream disconnect lost interrupted hold: %+v %v", held, err)
	}
}

func TestManagedTerminalExplicitOwnerControlBypassesOldHolds(t *testing.T) {
	s, target, _, owner := terminalSubmissionFixture(t)
	ctx := context.Background()
	original, err := s.store.AcquireActorOperation(ctx, target.UID, target.Incarnation, store.NewID(), "automatic", "terminal_stream")
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.store.AcquireOwnerActorOperation(ctx, target, store.NewID(), "owner:previous", "slash", owner, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishActorOperation(ctx, old, false); err != nil {
		t.Fatal(err)
	}
	token, _, err := auth.NewSessions(s.store, 0).Start(ctx, owner, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var effects atomic.Int32
	h := terminalSubmissionHandler(t, s, func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			if _, err := http.ReadRequest(bufio.NewReader(b)); err == nil {
				effects.Add(1)
			}
		}()
		return a, nil
	})
	h.ServeHTTP(terminalSubmissionWriter(t, io.Discard), terminalSubmissionRequest(target, token))
	if effects.Load() != 1 {
		t.Fatalf("explicit owner terminal blocked by old holds: %d", effects.Load())
	}
	held, err := s.store.CurrentActorOperation(ctx, target.UID)
	if err != nil || held.OperationID != original.OperationID || held.InterruptedBy != old.OperationID || held.State != "unconfirmed" {
		t.Fatalf("owner control erased previous evidence: %+v %v", held, err)
	}
}

func TestManagedTerminalOwnerInterruptsDelayedSubmission(t *testing.T) {
	s, target, token, owner := terminalSubmissionFixture(t)
	ctx := context.Background()
	dialed, resume, upstreamDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var effects, writes atomic.Int32
	h := terminalSubmissionHandler(t, s, func(context.Context, string, string) (net.Conn, error) {
		close(dialed)
		<-resume
		a, b := net.Pipe()
		go func() {
			defer close(upstreamDone)
			defer b.Close()
			if _, err := http.ReadRequest(bufio.NewReader(b)); err == nil {
				effects.Add(1)
			}
		}()
		return terminalSubmissionConn{Conn: a, beforeWrite: func() { writes.Add(1) }}, nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(terminalSubmissionWriter(t, io.Discard), terminalSubmissionRequest(target, token))
	}()
	select {
	case <-dialed:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal did not reach delayed dial")
	}
	release, err := s.managed.effects.acquire(ctx, target.UID)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.store.AcquireOwnerActorOperation(ctx, target, store.NewID(), "owner:test", "slash", owner, func(context.Context) error { return nil })
	release()
	if err != nil {
		close(resume)
		<-done
		t.Fatal(err)
	}
	held, err := s.store.CurrentActorOperation(ctx, target.UID)
	if err != nil || held.InterruptedBy != op.OperationID {
		close(resume)
		<-done
		t.Fatalf("owner did not interrupt original workspace claim: %+v %v", held, err)
	}
	close(resume)
	<-done
	<-upstreamDone
	if writes.Load() != 0 || effects.Load() != 0 {
		t.Fatalf("interrupted delayed terminal submitted writes=%d effects=%d", writes.Load(), effects.Load())
	}
	held, err = s.store.CurrentActorOperation(ctx, target.UID)
	if err != nil || held.InterruptedBy != op.OperationID || held.State != "unconfirmed" {
		t.Fatalf("interrupted evidence lost: %+v %v", held, err)
	}
}
