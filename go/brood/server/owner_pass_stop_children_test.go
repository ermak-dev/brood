package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: HTTP stop владельца замораживает диспетчер, но не отменяет детей.
// SPEC: Relay принимает поздний отчёт, а pending ребёнок больше не отправляется.
func TestOwnerPassStopRetainsGuestChildEvidence(t *testing.T) {
	d := newChildEngineServer(t)
	parent, worker, received := childRelayWithSession(t, d, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		return relay.SessionResponse{OK: true, Outcome: "completed", Text: "owner control"}
	})
	firstArgs := childGuestCreate(d, "pass-stop-running")
	firstArgs["total"] = 2
	first := childResponseID(t, childCall(t, parent, "child_create", firstArgs))
	secondArgs := childGuestCreate(d, "pass-stop-pending")
	secondArgs["total"] = 2
	second := childResponseID(t, childCall(t, parent, "child_create", secondArgs))
	d.srv.children.dispatch(context.Background())
	if received.Load() != 1 {
		t.Fatalf("expected exactly one active executor: %d", received.Load())
	}
	r, b, _ := d.do(d.alice, "POST", "/api/swarms/"+d.swarm+"/stop", d.csrf(d.alice), nil)
	expectStatus(t, "owner pass stop", r, b, http.StatusOK)
	d.srv.children.dispatch(context.Background())
	if received.Load() != 1 {
		t.Fatal("stopped dispatcher delivered pending child")
	}
	response := childCall(t, worker, "child_complete", map[string]any{"child_id": first, "request_id": "pass-stop-running", "outcome": "success", "report": "late independent child evidence"})
	if !response.OK {
		t.Fatalf("stopped pass rejected late evidence: %+v", response)
	}
	response = childCall(t, parent, "child_join", map[string]any{"parent_sequence": 1, "parent_request_id": firstArgs["parent_request_id"], "wait_seconds": 0})
	if response.Outcome != "unconfirmed" {
		t.Fatalf("stopped join claimed another outcome: %+v", response)
	}
	pending, err := d.srv.store.SwarmChildWorkByID(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Task.State != "pending" || pending.Task.Outcome != "" {
		t.Fatalf("stop cancelled or completed pending intent: %+v", pending.Task)
	}
	sw, err := d.srv.store.GetSwarm(context.Background(), d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	if sw.Execution != "stopped" || d.committed().State != "unconfirmed" {
		t.Fatalf("late evidence advanced stopped pass: %+v %+v", sw, d.committed())
	}
	r, b, _ = d.do(d.alice, "POST", "/api/swarms/"+d.swarm+"/start", d.csrf(d.alice), map[string]any{"definition": sw.DefinitionName, "goal": "new automatic pass", "commit": strings.Repeat("a", 40)})
	expectStatus(t, "restart retains unresolved obligations", r, b, http.StatusConflict)
	if b["detail"] != "outstanding child obligations require explicit reconciliation before automatic progression" {
		t.Fatalf("restart hides outstanding-obligation reason: %v", b)
	}
}
