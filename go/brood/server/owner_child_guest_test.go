package server

// BUG: A late child report can make the guest adapter claim a successful join
// after an owner interruption, although the store correctly retained uncertainty.
// HYPOTHESIS: The adapter treats open=0 as proof of parent progression.
// SOLUTION: Return interrupted=true and unconfirmed without joined/progression claims.
// RESULT: FIXED — authenticated late completion retains evidence, not parent success.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func TestOwnerInterruptedChildGuestNeverClaimsJoined(t *testing.T) {
	d := newChildEngineServer(t)
	parent, worker, _ := childRelayWithSession(t, d, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		return relay.SessionResponse{OK: true, Outcome: "completed", Text: "input acknowledged"}
	})
	args := childGuestCreate(d, "one")
	args["total"] = 1
	id := childResponseID(t, childCall(t, parent, "child_create", args))
	d.srv.children.dispatch(context.Background())
	r, b, _ := d.do(d.alice, "POST", "/api/commands/session_terminal", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": map[string]any{"name": d.worker.Name, "action": "interrupt", "tid": "owner"}})
	expectStatus(t, "explicit owner interruption", r, b, http.StatusOK)
	if response := childCall(t, worker, "child_complete", map[string]any{"child_id": id, "request_id": "one", "outcome": "success", "report": "Observed late executor evidence"}); !response.OK {
		t.Fatalf("late evidence refused: %+v", response)
	}
	response := childCall(t, parent, "child_join", map[string]any{"parent_sequence": 1, "parent_request_id": args["parent_request_id"], "wait_seconds": 0})
	var data struct {
		Joined      bool   `json:"joined"`
		Interrupted bool   `json:"interrupted"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Outcome != "unconfirmed" || data.Joined || !data.Interrupted || data.State != "unconfirmed" || strings.Contains(response.Text, "advanced its graph") {
		t.Fatalf("interrupted guest response claims parent success: %+v data=%+v", response, data)
	}
	if d.committed().State != "unconfirmed" {
		t.Fatal("late guest report advanced interrupted parent")
	}
}
