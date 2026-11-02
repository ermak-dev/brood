package server

import (
	"context"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

// BUG: HTTP failure status discards the safe unknown-outcome signal needed by CLI.
// SPEC: An unknown workspace effect carries fixed metadata and retains its exact reservation.
func TestCommandHTTPPreservesUnconfirmedOutcome(t *testing.T) {
	ts, alice, master, _ := masterSetup(t)
	ts.srv.managed.hub = commandSessionHub{guestRelay: ts.srv.managed.hub, call: func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
		return relay.SessionResponse{Text: "partial effect", Outcome: "unknown"}, nil
	}}
	response, body, _ := ts.do(alice, "POST", "/api/commands/restart", ts.csrf(alice), map[string]any{"swarm_id": master.SwarmID, "arguments": map[string]any{"name": "master", "confirm": true}})
	expectStatus(t, "unknown restart", response, body, 409)
	if got := response.Header.Get("X-Brood-Command-Outcome"); got != "unconfirmed" {
		t.Fatalf("safe outcome header=%q", got)
	}
	op, err := ts.srv.store.CurrentActorOperation(context.Background(), master.UID)
	if err != nil || op.State != "unconfirmed" {
		t.Fatal("unknown workspace operation was released")
	}
}
