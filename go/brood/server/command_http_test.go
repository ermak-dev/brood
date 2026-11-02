package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"brood.dev/go/brood/command"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: One authorized command service behind CLI HTTP and guest RPC.
// SPEC: Validate catalog arguments and scope before executing; worker permissions are not operator permissions.
func TestCommandHTTPProfileAndArgumentRefusal(t *testing.T) {
	ts, alice, master, _ := masterSetup(t)
	path := "/api/commands/profile"
	r, b, raw := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"swarm_id": master.SwarmID, "arguments": map[string]any{}})
	if r.StatusCode != http.StatusOK || b["text"] == "" {
		t.Fatalf("profile: status=%d body=%s", r.StatusCode, raw)
	}
	r, _, raw = ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"arguments": map[string]any{"uid": "forged"}})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown argument: status=%d body=%s", r.StatusCode, raw)
	}
	r, _, raw = ts.do(alice, "POST", "/api/commands/unknown", ts.csrf(alice), map[string]any{"arguments": map[string]any{}})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown command: status=%d body=%s", r.StatusCode, raw)
	}
	r, _, raw = ts.do(alice, "POST", path, "", map[string]any{"arguments": map[string]any{}})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("missing csrf: status=%d body=%s", r.StatusCode, raw)
	}
}

func TestCommandHTTPHumanMessageHasNoForgedActorIdentity(t *testing.T) {
	ts, alice, master, _ := masterSetup(t)
	called := false
	ts.srv.managed.deliverMessage = func(_ context.Context, uid string, generation int64, req relay.MessageRequest) (relay.MessageResponse, error) {
		called = true
		if uid != master.UID || generation != master.Incarnation || req.From != "operator:alice" || strings.Contains(req.Body, "mcp__brood__send(to=\"alice\"") {
			t.Fatalf("forged identity: %+v", req)
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	r, _, raw := ts.do(alice, "POST", "/api/commands/send", ts.csrf(alice), map[string]any{"swarm_id": master.SwarmID, "arguments": map[string]any{"to": "master", "message": "hello"}})
	if r.StatusCode != 200 || !called {
		t.Fatalf("send: %d %s called=%v", r.StatusCode, raw, called)
	}
}

func TestCommandHTTPRejectsAmbiguousEnvelope(t *testing.T) {
	for _, body := range []string{
		`{"arguments":{},"arguments":{}}`,
		`{"swarm_id":"first","swarm_id":"second","arguments":{}}`,
		`{"swarm_id":null,"arguments":{}}`,
		`{"arguments":{"uid":"forged"}}`,
		`{"arguments":{},"identity":"forged"}`,
		`{"arguments":{}} {"arguments":{}}`,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		_, args, ok := commandRequestBody(w, r)
		if ok {
			_, err := command.ParseTool("profile", args)
			if err == nil {
				t.Errorf("ambiguous envelope accepted: %s", body)
			}
		}
	}
}

func TestCanonicalGuestCommandAuthorization(t *testing.T) {
	ts, _, master, _ := masterSetup(t)
	ctx := context.Background()
	got := ts.srv.managed.handleSwarmRequest(ctx, master.UID, master.Incarnation, relay.SwarmRequest{Tool: "profile", Arguments: json.RawMessage(`{}`)})
	if !got.OK || !strings.Contains(got.Text, "master") {
		t.Fatalf("guest profile: %+v", got)
	}
	got = ts.srv.managed.handleSwarmRequest(ctx, master.UID, master.Incarnation+1, relay.SwarmRequest{Tool: "profile", Arguments: json.RawMessage(`{}`)})
	if got.OK {
		t.Fatal("stale incarnation accepted")
	}
	got = ts.srv.managed.handleSwarmRequest(ctx, master.UID, master.Incarnation, relay.SwarmRequest{Tool: "project_add", Arguments: json.RawMessage(`{"name":"forged"}`)})
	if got.OK {
		t.Fatal("guest acquired operator project authority")
	}
}
