package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/workflowpolicy"
)

// BUG: Nonempty policy оставляет допустимый следующий actor без пути разрешения.
// HYPOTHESIS: Dispatcher разрешает result только в exit/human, а entry — только первый actor.
func TestPolicyActorSuccessorWithNonmatchingPathsActuallyDispatches(t *testing.T) {
	f := newResultCollectionFixture(t)
	f.result = func(_ context.Context, _ relay.SessionRequest, result attest.ResultAttestation) (relay.SessionResponse, error) {
		result.Paths = []string{"docs/result.md"}
		signature, err := attest.SignResult(attest.ResultKey(guestToken(f.srv.managed.registry.relaySecret, f.parent.UID, f.parent.Incarnation)), result.ResultClaim)
		if err != nil {
			return relay.SessionResponse{}, err
		}
		result.Signature = signature
		body, err := json.Marshal(result)
		return relay.SessionResponse{OK: true, Outcome: "complete", Text: string(body)}, err
	}
	accepted := f.completion(t)
	if !accepted.OK || !f.acknowledge(t, accepted).OK {
		t.Fatal("completion was not acknowledged")
	}
	f.srv.steps.wait()
	steps, err := f.srv.store.SwarmSteps(context.Background(), f.swarm)
	if err != nil || len(steps) != 2 || steps[1].NodeID != "review" || steps[1].State != "inflight" || steps[1].ActorUID != f.master.UID {
		t.Fatalf("nonmatching policy stranded actor successor: steps=%+v error=%v", steps, err)
	}
	_, messages := f.dispatchServer.calls()
	if len(messages) != 2 || f.calls.Load() != 1 {
		t.Fatalf("successor repeated effects: messages=%d resultObservations=%d", len(messages), f.calls.Load())
	}
}

func TestPolicyActorAfterHumanWithNonmatchingActionsActuallyDispatches(t *testing.T) {
	d := newDispatchServer(t)
	ctx := context.Background()
	sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	root, err := d.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(dispatchGated, "role: build,", "role: build, actionClasses: [work],", 1)
	if _, err = d.srv.store.SyncWorkflowDefinitions(ctx, sw.ProjectID, root.ID, map[string]string{"gated": graph}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err = d.srv.store.SetProjectWorkflowPolicy(ctx, root.ID, sw.ProjectID, 0, []workflowpolicy.Rule{{ID: "protect-deploy", Actions: []string{"deploy"}, Require: workflowpolicy.Human}}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	d.start("gated")
	requests, err := d.srv.store.SwarmRequests(ctx, d.swarm)
	if err != nil || len(requests) != 1 {
		t.Fatalf("human request: %+v %v", requests, err)
	}
	response, body, _ := d.do(d.alice, "POST", "/api/swarms/"+d.swarm+"/requests/"+requests[0].ID+"/answer", d.csrf(d.alice), map[string]any{"outcome": "success"})
	expectStatus(t, "graph answer", response, body, http.StatusOK)
	d.srv.steps.wait()
	steps, err := d.srv.store.SwarmSteps(ctx, d.swarm)
	if err != nil || len(steps) != 2 || steps[1].NodeID != "build" || steps[1].State != "inflight" {
		t.Fatalf("nonmatching policy stranded human-to-actor transition: %+v %v", steps, err)
	}
	_, messages := d.calls()
	if len(messages) != 1 {
		t.Fatalf("expected one actor instruction, got %d", len(messages))
	}
}

func TestInitialPolicyExplicitDenialTerminatesWithoutGuestEffect(t *testing.T) {
	d := newDispatchServer(t)
	ctx := context.Background()
	sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	root, err := d.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(dispatchGraph, "role: build,", "role: build, actionClasses: [work],", 1)
	if _, err = d.srv.store.SyncWorkflowDefinitions(ctx, sw.ProjectID, root.ID, map[string]string{"deliver": graph}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err = d.srv.store.SetProjectWorkflowPolicy(ctx, root.ID, sw.ProjectID, 0, []workflowpolicy.Rule{{ID: "deny-work", Actions: []string{"work"}, Require: workflowpolicy.Deny}}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	d.start("deliver")
	current, err := d.srv.store.GetSwarm(ctx, d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := d.srv.store.SwarmSteps(ctx, d.swarm)
	if err != nil || current.Execution != "failed" || len(steps) != 1 || !strings.Contains(steps[0].Detail, "deny-work") {
		t.Fatalf("explicit denial remained pending or lost rule reason: execution=%s steps=%+v error=%v", current.Execution, steps, err)
	}
	attests, messages := d.calls()
	if len(attests) != 0 || len(messages) != 0 {
		t.Fatal("denied entry contacted guest")
	}
}
