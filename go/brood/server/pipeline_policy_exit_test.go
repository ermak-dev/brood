package server

import (
	"context"
	"net/http"
	"testing"

	"brood.dev/go/brood/store"
)

const policyExitGraph = `apiVersion: brood.dev/v1alpha1
name: deliver
start: build
exit: done
nodes:
  build: {type: actor, role: build, prompt: work}
  done: {type: exit}
edges:
  - {from: build, to: done}
goalGates: [done]
`

func TestPolicyExitStartupScanResumesBookkeepingWithoutAnotherGuestCall(t *testing.T) {
	f := newResultCollectionFixtureWithGraph(t, false, policyExitGraph)
	accepted := f.completion(t)
	if !accepted.OK || !f.acknowledge(t, accepted).OK {
		t.Fatal("completion was not acknowledged")
	}
	f.srv.steps.wait()
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
	if err := f.srv.steps.kickPending(ctx); err != nil {
		t.Fatal(err)
	}
	f.srv.steps.wait()
	swarm, err := f.srv.store.GetSwarm(ctx, f.swarm)
	if err != nil || swarm.Execution != "succeeded" || f.calls.Load() != 1 {
		t.Fatalf("startup lost confirmed bookkeeping or repeated observation: execution=%s calls=%d error=%v", swarm.Execution, f.calls.Load(), err)
	}
}

func TestPolicyExitRuntimeCollectsEvidenceThenWaitsAndAppliesOneAnswer(t *testing.T) {
	for _, answer := range []string{"yes", "no"} {
		t.Run(answer, func(t *testing.T) {
			f := newResultCollectionFixtureWithGraph(t, false, policyExitGraph)
			accepted := f.completion(t)
			if !accepted.OK || !f.acknowledge(t, accepted).OK {
				t.Fatal("completion was not acknowledged")
			}
			f.srv.steps.wait()
			candidate, err := f.srv.store.GetPipelinePolicyCandidate(context.Background(), f.swarm, 1, "result")
			if err != nil {
				t.Fatalf("observed exit result did not create a policy candidate: %v", err)
			}
			steps, err := f.srv.store.SwarmSteps(context.Background(), f.swarm)
			if err != nil || len(steps) != 1 || steps[0].Outcome != nil || f.calls.Load() != 1 {
				t.Fatal("exit advanced before policy answer or repeated result collection")
			}
			url := "/api/swarms/" + f.swarm + "/requests/policy:" + candidate.ID + "/answer"
			for range 2 {
				r, body, _ := f.do(f.alice, "POST", url, f.csrf(f.alice), map[string]any{"response": answer, "revision": 1})
				expectStatus(t, "terminal policy answer", r, body, http.StatusOK)
				f.srv.steps.wait()
			}
			steps, err = f.srv.store.SwarmSteps(context.Background(), f.swarm)
			if err != nil || steps[0].State != "completed" || steps[0].Outcome == nil || *steps[0].Outcome != "success" || steps[0].Report == "" || f.calls.Load() != 1 {
				t.Fatal("policy resolution rewrote actual outcome/report or retried guest effect")
			}
			swarm, err := f.srv.store.GetSwarm(context.Background(), f.swarm)
			if err != nil {
				t.Fatal(err)
			}
			if answer == "yes" && (len(steps) != 2 || swarm.Execution != "succeeded") {
				t.Fatal("confirmed result failed to create one terminal exit")
			}
			if answer == "no" && (len(steps) != 1 || swarm.Execution != "failed") {
				t.Fatal("rejected result created a successor or left execution active")
			}
		})
	}
}
