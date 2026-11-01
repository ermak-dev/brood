package server

import (
	"context"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func TestManagedSwarmRPCDerivesSenderAndFencesGeneration(t *testing.T) {
	ts, alice, _, swarm, pve, hub := managedSetup(t)
	hub.connected = true
	r, body, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "sender"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create sender", r, body, 201)
	uid := body["metadata"].(map[string]any)["uid"].(string)
	for range 4 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	pve.nextIDs = []int{106}
	r, body, _ = ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "peer"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create peer", r, body, 201)
	called := false
	ts.srv.managed.deliverMessage = func(_ context.Context, _ string, _ int64, request relay.MessageRequest) (relay.MessageResponse, error) {
		called = true
		if request.From != "sender" || request.To != "peer" || !strings.Contains(request.Body, "mcp__brood__send") {
			t.Errorf("delivery request: %+v", request)
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	ctx := context.Background()
	roster := ts.srv.managed.handleSwarmRequest(ctx, uid, 1, relay.SwarmRequest{Tool: "agents"})
	if !roster.OK || !strings.Contains(roster.Text, `"name":"peer"`) {
		t.Fatalf("roster: %+v", roster)
	}
	if response := ts.srv.managed.handleSwarmRequest(ctx, uid, 2, relay.SwarmRequest{Tool: "agents"}); response.OK {
		t.Fatal("retired incarnation served")
	}
	response := ts.srv.managed.handleSwarmRequest(ctx, uid, 1, relay.SwarmRequest{Tool: "send", To: "peer", Message: "hello"})
	if !response.OK || !called {
		t.Fatalf("send: %+v called=%v", response, called)
	}
	called = false
	response = ts.srv.managed.handleSwarmRequest(ctx, uid, 1, relay.SwarmRequest{Tool: "send", To: "foreign", Message: "hello"})
	if response.OK || called {
		t.Fatalf("foreign target accepted: %+v", response)
	}
}
