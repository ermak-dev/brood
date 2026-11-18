package store

// Начальная policy проверяется до workspace lease и повторяется у transport
// boundary. Исторический candidate/yes без текущего pass не разрешает эффект.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/workflowpolicy"
)

func TestInitialActorPolicyWaitsForExactHumanConfirmationBeforeReservation(t *testing.T) {
	ctx := context.Background()
	graph := strings.Replace(startBuildFirst, "role: build,", "role: build, actionClasses: [landing],", 1)
	s := policyPreviewStore(t, graph, []workflowpolicy.Rule{{ID: "land", Actions: []string{"landing"}, Require: workflowpolicy.Human}})
	if _, err := s.ReserveActorStep(ctx, "one", "before-human", "swarm:one"); !errors.Is(err, ErrPipelinePolicyPending) {
		t.Fatalf("initial protected actor reserved before human confirmation: %v", err)
	}
	if stepOf(t, s, 1).State != "pending" || stepOf(t, s, 1).ActorUID != "" {
		t.Fatal("policy wait was mislabeled as an actor attempt/refusal")
	}
	if _, err := s.CurrentActorOperation(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal("waiting policy acquired workspace lease")
	}
	candidate, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "entry")
	mustNil(t, err)
	if candidate.Preview.Decision.State != workflowpolicy.WaitingHuman {
		t.Fatal("policy refusal rolled back its durable candidate")
	}
	_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "yes"), allow)
	mustNil(t, err)
	attempt, err := s.ReserveActorStep(ctx, "one", "after-human", "swarm:one")
	mustNil(t, err)
	mustNil(t, s.CheckActorStepAttestation(ctx, attempt))
	if stepOf(t, s, 1).State != "attesting" || attempt.RequestID != "after-human" {
		t.Fatal("exact confirmation did not allow one current reservation")
	}
	if _, err := s.ReserveActorStep(ctx, "one", "duplicate", "swarm:one"); !errors.Is(err, ErrNoActorStep) {
		t.Fatalf("initial approval duplicated attempt: %v", err)
	}
	stopOne(t, s)
	if err := s.CheckActorStepAttestation(ctx, attempt); err == nil {
		t.Fatal("approval bypassed owner stop at final transport fence")
	}
}

func TestInitialActorPolicyApprovalCannotAuthorizeAnotherPass(t *testing.T) {
	ctx := context.Background()
	graph := strings.Replace(startBuildFirst, "role: build,", "role: build, actionClasses: [landing],", 1)
	s := policyPreviewStore(t, graph, []workflowpolicy.Rule{{ID: "land", Actions: []string{"landing"}, Require: workflowpolicy.Human}})
	_, err := s.ReserveActorStep(ctx, "one", "held-first", "swarm:one")
	if !errors.Is(err, ErrPipelinePolicyPending) {
		t.Fatal(err)
	}
	first, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "entry")
	mustNil(t, err)
	_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(first, "u", "yes"), allow)
	mustNil(t, err)
	stopOne(t, s)
	mustStart(t, s, "build-first")
	if _, err := s.ReserveActorStep(ctx, "one", "second-pass", "swarm:one"); !errors.Is(err, ErrPipelinePolicyPending) {
		t.Fatalf("old approval authorized another pass: %v", err)
	}
	second, err := s.GetPipelinePolicyCandidate(ctx, "one", 2, "entry")
	mustNil(t, err)
	if second.ID == first.ID || second.Preview.Policy.Pass != 2 {
		t.Fatal("new pass reused the first approval identity")
	}
}

func TestInitialActorPolicyFinalGuardRejectsUnprovenReservation(t *testing.T) {
	ctx := context.Background()
	graph := strings.Replace(startBuildFirst, "role: build,", "role: build, actionClasses: [landing],", 1)
	s := policyPreviewStore(t, graph, []workflowpolicy.Rule{{ID: "land", Actions: []string{"landing"}, Require: workflowpolicy.Human}})
	actor, err := s.GetManagedActor(ctx, "a")
	mustNil(t, err)
	mustNil(t, s.markStepAttesting(ctx, "one", 1, actor, "bypassed-reserver"))
	attempt := ActorStepAttempt{SwarmID: "one", Sequence: 1, NodeID: "build", RequestID: "bypassed-reserver", Actor: actor}
	if err := s.CheckActorStepAttestation(ctx, attempt); err == nil {
		t.Fatal("final guest boundary trusted a reservation without policy proof")
	}
	if err := s.RecordActorStepAttestation(ctx, attempt, startOrigin, startCommit); err == nil {
		t.Fatal("result recording accepted an unproven initial reservation")
	}
}

func TestInitialActorPolicyDenialUnknownAndRejectionNeverAcquireLease(t *testing.T) {
	for _, state := range []string{"deny", "unknown", "no"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			graph := strings.Replace(startBuildFirst, "role: build,", "role: build, actionClasses: [work],", 1)
			rules := []workflowpolicy.Rule{{ID: "work", Actions: []string{"work"}, Require: workflowpolicy.Human}}
			if state == "deny" {
				rules[0].Require = workflowpolicy.Deny
			}
			if state == "unknown" {
				graph = startBuildFirst
				rules = policyRules()
			}
			s := policyPreviewStore(t, graph, rules)
			_, err := s.ReserveActorStep(ctx, "one", "first", "swarm:one")
			want := ErrPipelinePolicyPending
			if state == "deny" {
				want = ErrActorStepRefused
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s did not enforce initial policy: %v", state, err)
			}
			if state == "no" {
				candidate, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "entry")
				mustNil(t, err)
				_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "no"), allow)
				mustNil(t, err)
			}
			if state == "deny" {
				want = ErrNoActorStep
			}
			if state == "no" {
				want = ErrActorStepRefused
			}
			if _, err := s.ReserveActorStep(ctx, "one", "retry", "swarm:one"); !errors.Is(err, want) {
				t.Fatalf("%s retry bypassed policy: %v", state, err)
			}
			w, err := s.GetSwarm(ctx, "one")
			mustNil(t, err)
			if state != "unknown" && (w.Execution != "failed" || stepOf(t, s, 1).State != "refused") {
				t.Fatal("explicit policy rejection remained pending")
			}
			if _, err := s.CurrentActorOperation(ctx, "a"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s acquired a workspace lease", state)
			}
		})
	}
}
