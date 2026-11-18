package store

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/workflowpolicy"
)

func TestPolicyLaterActorCannotUseGraphAnswerAsTransitionPermission(t *testing.T) {
	const graph = `apiVersion: brood.dev/v1alpha1
name: build-first
start: ask
exit: done
nodes:
  ask:
    type: human
    question: Continue?
    options:
      - {label: Continue, outcome: success}
  build: {type: actor, role: build, actionClasses: [landing], prompt: land}
  done: {type: exit}
edges:
  - {from: ask, to: build}
  - {from: build, to: done}
goalGates: [done]
`
	ctx := context.Background()
	s := policyPreviewStore(t, graph, []workflowpolicy.Rule{{ID: "land", Actions: []string{"landing"}, Require: workflowpolicy.Human}})
	requests, err := s.SwarmRequests(ctx, "one")
	mustNil(t, err)
	if len(requests) != 1 || requests[0].Origin != "node" {
		t.Fatal("initial graph question missing")
	}
	_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: requests[0].ID, UserID: "u", Outcome: "success"}, allow)
	mustNil(t, err)
	attempt, err := s.ReserveActorStep(ctx, "one", "unproved-later-entry", "swarm:one")
	if !errors.Is(err, ErrPipelinePolicyPending) || attempt.RequestID != "" {
		t.Fatalf("graph answer authorized a protected actor without candidate proof: attempt=%+v error=%v", attempt, err)
	}
	step := stepOf(t, s, 2)
	if step.State != "pending" || step.ActorUID != "" || step.RequestID != "" {
		t.Fatal("held later actor got an attempt")
	}
	if _, err := s.CurrentActorOperation(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("held later actor acquired workspace: %v", err)
	}
	actor, err := s.GetManagedActor(ctx, "a")
	mustNil(t, err)
	mustNil(t, s.markStepAttesting(ctx, "one", 2, actor, "bypassed-later-reserver"))
	unproved := ActorStepAttempt{SwarmID: "one", Sequence: 2, NodeID: "build", RequestID: "bypassed-later-reserver", Actor: actor}
	if err := s.CheckActorStepAttestation(ctx, unproved); !errors.Is(err, ErrPipelinePolicyPending) {
		t.Fatalf("final attestation boundary did not enforce later actor policy: %v", err)
	}
	if err := s.RecordActorStepAttestation(ctx, unproved, startOrigin, startCommit); !errors.Is(err, ErrPipelinePolicyPending) {
		t.Fatalf("attestation recording did not enforce later actor policy: %v", err)
	}
}
