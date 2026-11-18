package store

// BUG: независимое ревью выполнялось вне графа и не имело отрицательного исхода.
// HYPOTHESIS: отдельный review pass теряет protected-path evidence и автора artifact.
// SOLUTION: следующий graph review получает exact artifact, отдельный assignment
// и прежнего автора H; отрицательный исход не превращается в human confirmation.
// RESULT: FIXED — full Go и focused race подтверждают inline contract.
// STATUS: FIXED
// Полный Store race не завершился за 30m; этот отдельный gate не зелёный.

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/workflowpolicy"
)

const inlineReviewGraph = `apiVersion: brood.dev/v1alpha1
name: build-first
start: build
exit: done
nodes:
  build: {type: actor, role: build, actionClasses: [work], prompt: implement}
  review: {type: actor, role: review, actionClasses: [review], prompt: review}
  land: {type: actor, role: build, actionClasses: [landing], prompt: land}
  done: {type: exit}
edges:
  - {from: build, to: review}
  - {from: review, to: land}
  - {from: review, to: land, condition: outcome=fail}
  - {from: land, to: done}
goalGates: [done]
`

func inlineReviewStore(t *testing.T, graph, reviewer string, extra ...workflowpolicy.Rule) *Store {
	t.Helper()
	return inlineReviewStoreRules(t, graph, reviewer, append([]workflowpolicy.Rule{
		{ID: "storage", Paths: []string{"go/brood/store/**"}, Require: workflowpolicy.Human},
		{ID: "land-review", Actions: []string{"landing"}, Require: workflowpolicy.IndependentReview, ReviewRole: "review"},
	}, extra...))
}

func inlineReviewStoreRules(t *testing.T, graph, reviewer string, rules []workflowpolicy.Rule) *Store {
	t.Helper()
	s := goalStore(t)
	publish(t, s, "one", map[string]string{"build-first": graph})
	_, err := s.SetProjectWorkflowPolicy(context.Background(), "root", "one", 0, rules, allow)
	mustNil(t, err)
	if reviewer == "reviewer" {
		_, err := s.ReserveManagedActor(context.Background(), managedRequest(reviewer, "one", 202))
		mustNil(t, err)
		setActor(t, s, reviewer, "phase='running', stage='done', operation='start'")
	}
	bound := reviewer
	if bound == "" {
		bound = "a"
	}
	mustNil(t, s.BindSwarmRole(context.Background(), SwarmRoleBinding{SwarmID: "one", Role: "review", ActorUID: bound}))
	mustStart(t, s, "build-first")
	if reviewer == "" {
		_, err = s.db.Exec(`DELETE FROM swarm_role_bindings WHERE swarm_id='one' AND role='review'`)
		mustNil(t, err)
	}
	_, observation, signed := policyPreviewObserve(t, s)
	signed.Paths = []string{"go/brood/store/store.go"}
	signed.Signature, err = attest.SignResult(attest.ResultKey(resultTestToken), signed.ResultClaim)
	mustNil(t, err)
	mustNil(t, s.RecordPipelineResultEvidence(context.Background(), observation, attest.ResultKey(resultTestToken), signed))
	return s
}

func inlineApprove(t *testing.T, s *Store, sequence int64) PipelinePolicyTransitionResolution {
	t.Helper()
	ctx := context.Background()
	result, err := s.ResolvePipelinePolicyActor(ctx, "one", sequence)
	mustNil(t, err)
	if result.Decision.State == workflowpolicy.WaitingHuman {
		c, err := s.GetPipelinePolicyCandidateByID(ctx, "one", result.CandidateID)
		mustNil(t, err)
		_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(c, "u", "yes"), allow)
		mustNil(t, err)
		result, err = s.ResolvePipelinePolicyActor(ctx, "one", sequence)
		mustNil(t, err)
	}
	return result
}

func inlineObserveReview(t *testing.T, s *Store, outcome, head string, paths []string) ActorStepAttempt {
	t.Helper()
	return inlineObserveActor(t, s, outcome, head, paths, strings.Repeat("cd", 20), "inline-review")
}

func inlineObserveActor(t *testing.T, s *Store, outcome, head string, paths []string, base, id string, unsettled ...string) ActorStepAttempt {
	t.Helper()
	ctx := context.Background()
	a, err := s.ReserveActorStep(ctx, "one", id, "swarm:one")
	mustNil(t, err)
	if a.Source.Commit != base {
		t.Fatalf("actor workspace uses source %s instead of expected artifact %s", a.Source.Commit, base)
	}
	mustNil(t, s.RecordActorStepAttestation(ctx, a, startOrigin, a.Source.Commit))
	mustNil(t, s.FinishActorOperation(ctx, a.Lease, true))
	mustNil(t, s.FinishActorStepDelivery(ctx, a))
	receipt, err := s.AcceptPipelineCompletion(ctx, ActorCompletion{SwarmID: "one", Sequence: a.Sequence, RequestID: a.RequestID, ActorUID: a.Actor.UID, Incarnation: a.Actor.Incarnation, Outcome: outcome, Report: "Actual inline review: " + outcome}, base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(id, 32)[:32])), allow)
	mustNil(t, err)
	_, err = s.AcknowledgePipelineCompletion(ctx, receiptAck(receipt), allow)
	mustNil(t, err)
	i := receipt.Intent
	request := attest.ResultRequest{Claim: attest.Claim{ActorUID: i.ActorUID, Generation: i.Incarnation, Swarm: i.SwarmID, Step: i.RequestID, Origin: i.Origin, Commit: i.BaseCommit, Challenge: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}, Pass: i.Pass, Sequence: i.Sequence}
	o, err := s.ReservePipelineResultObservation(ctx, "one", a.Sequence, id+"-observe", "swarm:one", request)
	mustNil(t, err)
	if len(unsettled) > 0 {
		mustNil(t, s.SettlePipelineResultObservation(ctx, o, unsettled[0] == "refused", "review root result "+unsettled[0]))
		return a
	}
	signed := signedResult(t, request)
	signed.Head = head
	signed.Paths = paths
	signed.Signature, err = attest.SignResult(attest.ResultKey(resultTestToken), signed.ResultClaim)
	mustNil(t, err)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, o, attest.ResultKey(resultTestToken), signed))
	return a
}

func TestPolicyInlineReviewCombinedProtectedPathsAndLanding(t *testing.T) {
	ctx := context.Background()
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
	first := inlineApprove(t, s, 1)
	if !first.Applied || first.Rejected {
		t.Fatalf("legitimate next review blocked: %+v", first)
	}
	inlineObserveReview(t, s, "success", strings.Repeat("cd", 20), []string{})
	result := inlineApprove(t, s, 2)
	if !result.Applied || result.Rejected || result.Decision.State != workflowpolicy.Allowed {
		t.Fatalf("inline review not accepted for original work author: %+v", result)
	}
	var retained int
	mustNil(t, s.db.QueryRow(`SELECT COUNT(*) FROM pipeline_policy_review_evidence`).Scan(&retained))
	if retained != 1 {
		t.Fatal("actual inline review result was not durably retained as review evidence")
	}
	land, err := s.ReserveActorStep(ctx, "one", "inline-land", "swarm:one")
	mustNil(t, err)
	if land.Actor.UID != "a" {
		t.Fatalf("landing actor changed: %+v", land)
	}
	work, err := s.GetSwarmStep(ctx, "one", 1)
	mustNil(t, err)
	review, err := s.GetSwarmStep(ctx, "one", 2)
	mustNil(t, err)
	if work.Outcome == nil || *work.Outcome != "success" || !strings.Contains(work.Report, "Only docs") || review.Report != "Actual inline review: success" {
		t.Fatal("actual work/review reports lost")
	}
	_, _, base, err := s.stepPass(ctx, "one", 2)
	mustNil(t, err)
	if base != startCommit {
		t.Fatal("review rewrote immutable pass source")
	}
}

func TestPolicyInlineReviewUnavailableFailsInsteadOfWaiting(t *testing.T) {
	for _, reviewer := range []string{"a", ""} {
		t.Run("reviewer-"+reviewer, func(t *testing.T) {
			s := inlineReviewStore(t, inlineReviewGraph, reviewer)
			result := inlineApprove(t, s, 1)
			w, err := s.GetSwarm(context.Background(), "one")
			mustNil(t, err)
			step, err := s.GetSwarmStep(context.Background(), "one", 1)
			mustNil(t, err)
			if !result.Applied || !result.Rejected || w.Execution != "failed" || !strings.Contains(step.Detail, "review") || stepCount(t, s) != 1 {
				t.Fatalf("unavailable next reviewer not terminal: result=%+v swarm=%s step=%+v", result, w.Execution, step)
			}
			if step.Outcome == nil || *step.Outcome != "success" {
				t.Fatal("unavailable reviewer invented work failure")
			}
		})
	}
}

func TestPolicyInlineReviewNegativeAndChangedArtifactCannotLand(t *testing.T) {
	for _, state := range []string{"fail", "changed"} {
		t.Run(state, func(t *testing.T) {
			s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
			inlineApprove(t, s, 1)
			outcome, head, paths := "success", strings.Repeat("cd", 20), []string{}
			if state == "fail" {
				outcome = "fail"
			} else {
				head = strings.Repeat("ef", 20)
				paths = []string{"go/brood/store/changed.go"}
			}
			inlineObserveReview(t, s, outcome, head, paths)
			result := inlineApprove(t, s, 2)
			w, err := s.GetSwarm(context.Background(), "one")
			mustNil(t, err)
			review, err := s.GetSwarmStep(context.Background(), "one", 2)
			mustNil(t, err)
			if !result.Applied || !result.Rejected || w.Execution != "failed" || stepCount(t, s) != 2 {
				t.Fatalf("invalid review not terminal: %+v execution=%s", result, w.Execution)
			}
			if review.Outcome == nil || *review.Outcome != outcome || review.Report != "Actual inline review: "+outcome {
				t.Fatal("policy fabricated reviewer outcome")
			}
		})
	}
}

func TestPolicyInlineReviewMissingGraphStepCannotUseOutOfBandAuthority(t *testing.T) {
	s := inlineReviewStore(t, policyActorGraph, "reviewer")
	result := inlineApprove(t, s, 1)
	w, err := s.GetSwarm(context.Background(), "one")
	mustNil(t, err)
	if !result.Applied || !result.Rejected || w.Execution != "failed" {
		t.Fatalf("build->land waits for out-of-band review: %+v execution=%s", result, w.Execution)
	}
}

func TestPolicyInlineReviewUnknownDeliveryRemainsHeld(t *testing.T) {
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
	inlineApprove(t, s, 1)
	a, err := s.ReserveActorStep(context.Background(), "one", "unknown-review", "swarm:one")
	mustNil(t, err)
	if a.Source.Commit != strings.Repeat("cd", 20) {
		t.Fatal("review not pinned")
	}
	mustNil(t, s.RecordActorStepAttestation(context.Background(), a, startOrigin, a.Source.Commit))
	mustNil(t, s.FinishActorOperation(context.Background(), a.Lease, true))
	mustNil(t, s.UnconfirmActorStep(context.Background(), a, "delivery outcome unknown"))
	before := dumpTables(t, s, "swarm_steps", "pipeline_policy_review_assignments")
	resolution, err := s.ResolvePipelinePolicyReview(context.Background(), "one", 2)
	mustNil(t, err)
	if resolution.Applied || resolution.Rejected {
		t.Fatal("unknown delivery declared negative")
	}
	mustNil(t, s.RecoverActorSteps(context.Background()))
	if before != dumpTables(t, s, "swarm_steps", "pipeline_policy_review_assignments") {
		t.Fatal("unknown delivery replaced or changed after recovery")
	}
	_, err = s.ReserveActorStep(context.Background(), "one", "retry-review", "swarm:one")
	if err == nil || errors.Is(err, ErrActorStepRefused) {
		t.Fatalf("unknown delivery retried or declared refused: %v", err)
	}
	w, err := s.GetSwarm(context.Background(), "one")
	mustNil(t, err)
	if w.Execution == "failed" {
		t.Fatal("unknown delivery invented negative review")
	}
}
