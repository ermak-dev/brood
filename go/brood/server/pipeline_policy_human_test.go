package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

const policyHumanServerGraph = `apiVersion: brood.dev/v1alpha1
name: deliver
start: build
exit: done
nodes:
  build: {type: actor, role: build, prompt: work}
  ask:
    type: human
    question: Publish this artifact?
    options:
      - {label: Publish, outcome: success}
  done: {type: exit}
edges:
  - {from: build, to: ask}
  - {from: ask, to: done}
goalGates: [done]
`

func TestPolicyHumanRuntimeSeparatesPolicyAnswerFromGraphQuestion(t *testing.T) {
	for _, answer := range []string{"yes", "no"} {
		t.Run(answer, func(t *testing.T) {
			f := newResultCollectionFixtureWithGraph(t, false, policyHumanServerGraph)
			accepted := f.completion(t)
			if !accepted.OK || !f.acknowledge(t, accepted).OK {
				t.Fatal("completion receipt rejected")
			}
			f.srv.steps.wait()
			ctx := context.Background()
			candidate, err := f.srv.store.GetPipelinePolicyCandidate(ctx, f.swarm, 1, "result")
			if err != nil {
				t.Fatalf("observed human successor did not retain policy candidate: %v", err)
			}
			requests, err := f.srv.store.SwarmRequests(ctx, f.swarm)
			if err != nil || len(requests) != 1 || requests[0].Origin != "policy" {
				t.Fatal("human question appeared before policy release")
			}
			url := "/api/swarms/" + f.swarm + "/requests/policy:" + candidate.ID + "/answer"
			for range 2 {
				r, body, _ := f.do(f.alice, "POST", url, f.csrf(f.alice), map[string]any{"response": answer, "revision": 1})
				expectStatus(t, "human successor policy answer", r, body, http.StatusOK)
				f.srv.steps.wait()
			}
			steps, err := f.srv.store.SwarmSteps(ctx, f.swarm)
			if err != nil || steps[0].Outcome == nil || *steps[0].Outcome != "success" || steps[0].Report == "" || f.calls.Load() != 1 {
				t.Fatal("policy resolution changed actual result or repeated guest effect")
			}
			swarm, err := f.srv.store.GetSwarm(ctx, f.swarm)
			if err != nil {
				t.Fatal(err)
			}
			requests, err = f.srv.store.SwarmRequests(ctx, f.swarm)
			if err != nil {
				t.Fatal(err)
			}
			if answer == "yes" {
				if len(steps) != 2 || steps[1].NodeID != "ask" || steps[1].State != "waiting" || swarm.Execution != "waiting" {
					t.Fatal("policy yes failed to open exactly one waiting human visit")
				}
				var questions int
				for _, request := range requests {
					if request.Origin == "node" {
						questions++
						if request.State != "open" || request.Sequence == nil || *request.Sequence != 2 || !strings.Contains(request.Context, steps[0].Report) {
							t.Fatal("policy yes also answered or lost human question material")
						}
					}
				}
				if questions != 1 {
					t.Fatal("expected one separate unanswered graph question")
				}
			} else if len(steps) != 1 || swarm.Execution != "failed" || len(requests) != 1 {
				t.Fatal("policy no opened a human successor")
			}
		})
	}
}

func TestPolicyHumanStartupResumesOnlyObservedConfirmedBookkeeping(t *testing.T) {
	f := newResultCollectionFixtureWithGraph(t, false, policyHumanServerGraph)
	accepted := f.completion(t)
	if !accepted.OK || !f.acknowledge(t, accepted).OK {
		t.Fatal("completion receipt rejected")
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
	if err != nil || swarm.Execution != "waiting" || f.calls.Load() != 1 {
		t.Fatal("startup lost confirmed human boundary or repeated observation")
	}
}
