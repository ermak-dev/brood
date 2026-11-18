package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func acceptTerminalCompletion(t *testing.T, f *resultCollectionFixture, outcome string) relay.SwarmResponse {
	t.Helper()
	steps, err := f.srv.store.SwarmSteps(context.Background(), f.swarm)
	if err != nil || len(steps) != 1 {
		t.Fatalf("terminal fixture lacks one live source: %v", err)
	}
	arguments, err := json.Marshal(map[string]any{"run_id": f.swarm, "sequence": steps[0].Sequence, "request_id": steps[0].RequestID, "outcome": outcome, "report": "Exact terminal report, not a policy decision."})
	if err != nil {
		t.Fatal(err)
	}
	accepted := f.srv.managed.handleSwarmRequest(context.Background(), f.parent.UID, f.parent.Incarnation, relay.SwarmRequest{Tool: "pipeline_visit_complete", Arguments: arguments})
	if !accepted.OK || !f.acknowledge(t, accepted).OK {
		t.Fatalf("terminal completion receipt was not acknowledged: %+v", accepted)
	}
	f.srv.steps.wait()
	return accepted
}

func TestTerminalPolicyRuntimeRetainsActualOutcomeWithoutExitOrRepeatedObservation(t *testing.T) {
	for _, outcome := range []string{"success", "fail"} {
		for _, answer := range []string{"yes", "no"} {
			t.Run(outcome+"/"+answer, func(t *testing.T) {
				opposite := "success"
				if outcome == "success" {
					opposite = "fail"
				}
				graph := strings.Replace(policyExitGraph, "{from: build, to: done}", "{from: build, to: done, condition: outcome="+opposite+"}", 1)
				f := newResultCollectionFixtureWithGraph(t, false, graph)
				acceptTerminalCompletion(t, f, outcome)
				ctx := context.Background()
				candidate, err := f.srv.store.GetPipelinePolicyCandidate(ctx, f.swarm, 1, "result")
				if err != nil || !candidate.Preview.Transition.Terminal || candidate.Preview.Transition.Next != "" || f.calls.Load() != 1 {
					t.Fatalf("terminal result is stuck or invented an exit: %+v, calls=%d, error=%v", candidate, f.calls.Load(), err)
				}
				steps, err := f.srv.store.SwarmSteps(ctx, f.swarm)
				if err != nil || len(steps) != 1 || steps[0].Outcome != nil {
					t.Fatal("terminal result finalized before policy confirmation")
				}
				url := "/api/swarms/" + f.swarm + "/requests/policy:" + candidate.ID + "/answer"
				for range 2 {
					r, body, _ := f.do(f.alice, "POST", url, f.csrf(f.alice), map[string]any{"response": answer, "revision": 1})
					expectStatus(t, "terminal no-edge policy answer", r, body, http.StatusOK)
					f.srv.steps.wait()
				}
				steps, err = f.srv.store.SwarmSteps(ctx, f.swarm)
				if err != nil || len(steps) != 1 || steps[0].State != "completed" || steps[0].Outcome == nil || *steps[0].Outcome != outcome || steps[0].Report != "Exact terminal report, not a policy decision." || f.calls.Load() != 1 {
					t.Fatalf("terminal confirmation rewrote the report or repeated guest work: steps=%+v calls=%d error=%v", steps, f.calls.Load(), err)
				}
				w, err := f.srv.store.GetSwarm(ctx, f.swarm)
				if err != nil || w.Execution != "failed" {
					t.Fatalf("missing graph edge became success: execution=%s error=%v", w.Execution, err)
				}
			})
		}
	}
}

func TestTerminalPolicyStartupRecoveryUsesRetainedEvidenceWithoutAnotherGuestCall(t *testing.T) {
	f := newResultCollectionFixtureWithGraph(t, false, policyExitGraph)
	acceptTerminalCompletion(t, f, "fail")
	ctx := context.Background()
	candidate, err := f.srv.store.GetPipelinePolicyCandidate(ctx, f.swarm, 1, "result")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := f.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.srv.store.AnswerPipelinePolicy(ctx, store.PipelinePolicyAnswer{SwarmID: f.swarm, CandidateID: candidate.ID, UserID: owner.ID, ExpectedRevision: 1, Answer: "yes"}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.srv.steps.kickPending(ctx); err != nil {
			t.Fatal(err)
		}
		f.srv.steps.wait()
	}
	steps, err := f.srv.store.SwarmSteps(ctx, f.swarm)
	if err != nil || len(steps) != 1 || steps[0].Outcome == nil || *steps[0].Outcome != "fail" || f.calls.Load() != 1 {
		t.Fatalf("startup repeated observation or lost terminal proof: %+v calls=%d error=%v", steps, f.calls.Load(), err)
	}
	w, err := f.srv.store.GetSwarm(ctx, f.swarm)
	if err != nil || w.Execution != "failed" {
		t.Fatalf("startup failed to settle terminal source: %s, %v", w.Execution, err)
	}
}
