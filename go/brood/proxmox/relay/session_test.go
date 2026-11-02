package relay

// FEATURE: Bounded session RPC on a dedicated authenticated SSH channel.
// SPEC: Preserve Caller, reject oversized frames, cancel independently, and
// reject responses when authorization or peer identity changes during a call.
// IMPLEMENTATION: Independent SSH channels, bounded frames and before/after fences.
// RESULT: IMPLEMENTED — transport tests cover cancellation, revocation and limits.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// FEATURE: Connection-scoped session handler state.
// SPEC: Factory runs after handshake and its context ends on disconnect.
func TestSessionFactoryConnectionLifetime(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	initialized := make(chan context.Context, 1)
	cfg.SessionFactory = func(ctx context.Context) func(context.Context, SessionRequest) SessionResponse {
		initialized <- ctx
		return func(context.Context, SessionRequest) SessionResponse {
			return SessionResponse{OK: true, Outcome: "complete", Text: "connection-owned"}
		}
	}
	start(t, f, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var connection context.Context
	select {
	case connection = <-initialized:
	case <-ctx.Done():
		t.Fatal("factory not initialized")
	}
	got, err := f.h.CallSession(ctx, "actor-uid", 3, SessionRequest{Operation: "tail", Caller: "a", Arguments: json.RawMessage(`{}`)})
	if err != nil || !got.OK || got.Text != "connection-owned" {
		t.Fatal(got, err)
	}
	f.h.Disconnect("actor-uid", 3)
	select {
	case <-connection.Done():
	case <-ctx.Done():
		t.Fatal("connection state survived disconnect")
	}
}

func TestSessionRoundtripAndRevocation(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	entered, release := make(chan struct{}, 1), make(chan struct{})
	cfg.Session = func(ctx context.Context, req SessionRequest) SessionResponse {
		if req.Caller != "user:42" || req.Operation != "tail" {
			t.Errorf("request: %#v", req)
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return SessionResponse{OK: true, Text: "screen", Outcome: "complete"}
	}
	start(t, f, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := f.h.CallSession(ctx, "actor-uid", 3, SessionRequest{Operation: "tail", Arguments: json.RawMessage(`{"name":"ignored"}`), Caller: "user:42"})
		result <- err
	}()
	<-entered
	f.h.Disconnect("actor-uid", 3)
	close(release)
	if err := <-result; err == nil {
		t.Fatal("stale session response accepted")
	}
}

func TestSessionAuthorizationRecheckedAfterResponse(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.Session = func(context.Context, SessionRequest) SessionResponse {
		f.current.Store(false)
		return SessionResponse{OK: true, Text: "private", Outcome: "complete"}
	}
	start(t, f, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := f.h.CallSession(ctx, "actor-uid", 3, SessionRequest{Operation: "tail", Arguments: json.RawMessage(`{}`), Caller: "a"})
	if err == nil || got.Text != "" {
		t.Fatalf("revoked response leaked: %#v %v", got, err)
	}
}
func TestSessionOversizedResponseIsExplicitFailure(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.Session = func(context.Context, SessionRequest) SessionResponse {
		return SessionResponse{OK: true, Text: strings.Repeat("x", MaxSessionBytes), Outcome: "complete"}
	}
	start(t, f, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := f.h.CallSession(ctx, "actor-uid", 3, SessionRequest{Operation: "tail", Arguments: json.RawMessage(`{}`), Caller: "a"})
	if err != nil || got.OK || got.Outcome != "failed" || !strings.Contains(got.Text, "limit") {
		t.Fatalf("%#v %v", got, err)
	}
}
func TestSessionRoundtrip(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.Session = func(_ context.Context, req SessionRequest) SessionResponse {
		return SessionResponse{OK: true, Text: req.Caller, Outcome: "complete"}
	}
	start(t, f, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := SessionRequest{Operation: "tail", Arguments: json.RawMessage(`{}`), Caller: "user:42"}
	got, err := f.h.CallSession(ctx, "actor-uid", 3, req)
	if err != nil || !got.OK || got.Text != "user:42" {
		t.Fatalf("%#v %v", got, err)
	}
	req.Caller = strings.Repeat("a", MaxSessionBytes)
	if _, err := f.h.CallSession(ctx, "actor-uid", 3, req); err == nil {
		t.Fatal("oversized request accepted")
	}
}

func TestSessionCanceledChannelDoesNotSerialize(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	entered, stopped := make(chan struct{}), make(chan struct{})
	cfg.Session = func(ctx context.Context, req SessionRequest) SessionResponse {
		if req.Operation == "slow" {
			close(entered)
			<-ctx.Done()
			close(stopped)
		}
		return SessionResponse{OK: true, Outcome: "complete"}
	}
	start(t, f, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.h.CallSession(ctx, "actor-uid", 3, SessionRequest{Operation: "slow", Arguments: json.RawMessage(`{}`), Caller: "a"})
		done <- err
	}()
	<-entered
	fast, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := f.h.CallSession(fast, "actor-uid", 3, SessionRequest{Operation: "tail", Arguments: json.RawMessage(`{}`), Caller: "a"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-fast.Done():
		t.Fatal("call ignored cancellation")
	}
	select {
	case <-stopped:
	case <-fast.Done():
		t.Fatal("helper outlived channel")
	}
}
