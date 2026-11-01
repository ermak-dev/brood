package relay

import (
	"context"
	"testing"
	"time"
)

// FEATURE: Outbound swarm RPC uses the current authenticated guest SSH session.
// SPEC: the server derives identity; malformed requests and revoked peers fail closed.
func TestInboundMessageRechecksCurrentAuthorization(t *testing.T) {
	f := setup(t)
	f.h.SetMessageRouter(func(string, MessageRequest) MessageResponse { return MessageResponse{OK: true, Verdict: "unexpected"} })
	conn, _ := rawSSH(t, f, testToken)
	f.current.Store(false)
	frame, _ := encodeMessageFrame(MessageRequest{To: "peer", From: "actor", Body: "hello"})
	if channel, _, err := conn.OpenChannel(messageChannel, frame); err == nil {
		channel.Close()
		t.Fatal("revoked connection opened a new message channel")
	}
}

// FEATURE: The bound swarm relay accepts narrow add requests.
// SPEC: Only tool and a DNS-label actor name are allowed; identity stays bound.
func TestDecodeAddRequest(t *testing.T) {
	for _, tc := range []struct {
		wire  string
		valid bool
	}{
		{`{"tool":"add","to":"peer-1"}`, true},
		{`{"tool":"add","to":"a"}`, true},
		{`{"tool":"add"}`, false},
		{`{"tool":"add","to":"Peer"}`, false},
		{`{"tool":"add","to":"peer-"}`, false},
		{`{"tool":"add","to":"../peer"}`, false},
		{`{"tool":"add","to":"peer","message":"hello"}`, false},
		{`{"tool":"add","to":"peer","wait_seconds":1}`, false},
		{`{"tool":"add","to":"peer","role":"master"}`, false},
		{`{"tool":"add","to":"peer","pool":"other"}`, false},
		{`{"tool":"add","to":"peer","token":"secret"}`, false},
		{`{"tool":"add","to":"peer","actor_uid":"other"}`, false},
		{`{"tool":"add","to":"peer","generation":4}`, false},
	} {
		if _, valid := DecodeSwarmRequest([]byte(tc.wire)); valid != tc.valid {
			t.Errorf("valid=%v for %s", valid, tc.wire)
		}
	}
}

func TestSwarmRPCUsesAuthenticatedCurrentSession(t *testing.T) {
	f := setup(t)
	calls := make(chan SwarmCaller, 2)
	f.cfg.OnSwarmSession = func(ctx context.Context, call SwarmCaller) { calls <- call; <-ctx.Done() }
	f.h.SetSwarmHandler(func(ctx context.Context, uid string, generation int64, req SwarmRequest) SwarmResponse {
		if uid != "actor-uid" || generation != 3 {
			t.Errorf("wrong identity %s/%d", uid, generation)
		}
		return SwarmResponse{OK: true, Text: req.Tool + " from authenticated actor"}
	})
	start(t, f, f.cfg)
	var call SwarmCaller
	select {
	case call = <-calls:
	case <-time.After(3 * time.Second):
		t.Fatal("no session caller")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got := call(ctx, SwarmRequest{Tool: "agents"})
	if !got.OK || got.Text != "agents from authenticated actor" {
		t.Fatalf("RPC: %+v", got)
	}
	if got := call(ctx, SwarmRequest{Tool: "add", To: "peer-1"}); !got.OK || got.Text != "add from authenticated actor" {
		t.Fatalf("add RPC: %+v", got)
	}
	if got := call(ctx, SwarmRequest{Tool: "exec"}); got.OK {
		t.Fatal("unknown operation accepted")
	}
	if got := call(ctx, SwarmRequest{Tool: "send", To: "peer", Message: "hello", WaitSeconds: 301}); got.OK {
		t.Fatal("unbounded wait accepted")
	}
	f.current.Store(false)
	if got := call(ctx, SwarmRequest{Tool: "add", To: "peer-2"}); got.OK {
		t.Fatal("revoked peer created an actor")
	}
	if got := call(ctx, SwarmRequest{Tool: "agents"}); got.OK {
		t.Fatal("revoked peer still served")
	}
}
