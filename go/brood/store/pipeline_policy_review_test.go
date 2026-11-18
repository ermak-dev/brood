package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/workflowpolicy"
)

// BUG: внешний review pass принимался вместо следующего review узла графа.
// HYPOTHESIS: assignment должен быть привязан к текущему inline visit и автору H.
// SOLUTION: реальный graph review и root evidence остаются единственным источником.
// RESULT: FIXED — история, restart, current authority и forged claims проверены.
// STATUS: FIXED
func TestPolicyReviewHistorySurvivesRestartAndRejectsSameActorForgery(t *testing.T) {
	ctx := context.Background()
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
	first := inlineApprove(t, s, 1)
	requirements, err := s.PipelinePolicyReviewRequirements(ctx, "one", first.CandidateID)
	mustNil(t, err)
	if len(requirements) != 1 || requirements[0].ProducerUID != "a" || requirements[0].Artifact != strings.Repeat("cd", 20) || requirements[0].Role != "review" {
		t.Fatalf("incorrect original work lineage: %+v", requirements)
	}
	assignment, err := s.GetPipelinePolicyReviewAssignment(ctx, "one", first.CandidateID, "land-review")
	mustNil(t, err)
	forged := assignment
	forged.ActorUID = requirements[0].ProducerUID
	_, _, err = s.checkedPipelinePolicyReview(ctx, requirements[0], forged)
	if !errors.Is(err, ErrSwarmRoleMismatch) {
		t.Fatalf("same author review accepted: %v", err)
	}
	inlineObserveReview(t, s, "success", strings.Repeat("cd", 20), []string{})
	mustNil(t, s.RecordPipelinePolicyReviewEvidence(ctx, "one", first.CandidateID, "land-review"))
	tables := []string{"pipeline_policy_candidates", "pipeline_policy_review_requirements", "pipeline_policy_review_assignments", "pipeline_policy_review_evidence"}
	before := dumpTables(t, s, tables...)
	for _, table := range tables {
		for _, query := range []string{"UPDATE " + table + " SET " + map[string]string{"pipeline_policy_candidates": "id=id", "pipeline_policy_review_requirements": "role=role", "pipeline_policy_review_assignments": "actor_uid=actor_uid", "pipeline_policy_review_evidence": "observation_id=observation_id"}[table], "DELETE FROM " + table, "INSERT OR REPLACE INTO " + table + " SELECT * FROM " + table} {
			if _, err := s.db.Exec(query); err == nil {
				t.Fatalf("immutable history accepted %s", query)
			}
		}
	}
	if before != dumpTables(t, s, tables...) {
		t.Fatal("forgery changed history")
	}
	var seq int
	var name, path string
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, path)
	mustNil(t, err)
	defer reopened.Close()
	if before != dumpTables(t, reopened, tables...) {
		t.Fatal("restart rewrote history")
	}
	result := inlineApprove(t, reopened, 2)
	if !result.Applied || result.Rejected || result.Decision.State != workflowpolicy.Allowed {
		t.Fatalf("restart lost inline review: %+v", result)
	}
}

func TestPolicyReviewAssignmentFinalFenceHonorsCurrentAuthorityAndSourceStop(t *testing.T) {
	for _, change := range []string{"stop", "incarnation", "unbound", "manual"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
			first := inlineApprove(t, s, 1)
			mustNil(t, s.CheckPipelinePolicyReviewAssignment(ctx, "one", first.CandidateID, "land-review"))
			a, err := s.ReserveActorStep(ctx, "one", "review-fence", "swarm:one")
			mustNil(t, err)
			switch change {
			case "stop":
				stopOne(t, s)
			case "incarnation":
				setActor(t, s, "reviewer", "incarnation=incarnation+1")
			case "unbound":
				_, err = s.db.Exec(`DELETE FROM swarm_role_bindings WHERE swarm_id='one' AND role='review'`)
				mustNil(t, err)
			case "manual":
				actor, err := s.GetManagedActor(ctx, "reviewer")
				mustNil(t, err)
				_, err = s.AcquireOwnerActorOperation(ctx, actor, "owner-review", "operator:root", "stop", "root", func(context.Context) error { return nil })
				mustNil(t, err)
			}
			if err := s.CheckPipelinePolicyReviewAssignment(ctx, "one", first.CandidateID, "land-review"); err == nil {
				t.Fatal("review assignment outlived authority")
			}
			if err := s.CheckActorStepAttestation(ctx, a); err == nil {
				t.Fatal("final guest fence outlived authority")
			}
		})
	}
}

func TestPolicyHumanRejectionIsTerminalEvenWhenPriorReviewAuthorityIsRevoked(t *testing.T) {
	ctx := context.Background()
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
	inlineApprove(t, s, 1)
	inlineObserveReview(t, s, "success", strings.Repeat("cd", 20), []string{})
	result, err := s.ResolvePipelinePolicyActor(ctx, "one", 2)
	mustNil(t, err)
	_, err = s.AnswerPipelinePolicy(ctx, PipelinePolicyAnswer{SwarmID: "one", CandidateID: result.CandidateID, UserID: "u", ExpectedRevision: 1, Answer: "no"}, allow)
	mustNil(t, err)
	setActor(t, s, "reviewer", "incarnation=incarnation+1")
	result, err = s.ResolvePipelinePolicyActor(ctx, "one", 2)
	mustNil(t, err)
	w, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	if !result.Applied || !result.Rejected || w.Execution != "failed" || stepCount(t, s) != 2 {
		t.Fatalf("explicit rejection remained pending: %+v", result)
	}
}

func TestPolicyIndependentReviewCannotImportSeparatePassOrUnobservedClaims(t *testing.T) {
	ctx := context.Background()
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
	first := inlineApprove(t, s, 1)
	if err := s.RecordPipelinePolicyReviewEvidence(ctx, "one", first.CandidateID, "land-review"); err == nil {
		t.Fatal("unobserved review accepted")
	}
	_, err := s.AssignPipelinePolicyReview(ctx, "one", first.CandidateID, "land-review", "two", 1)
	if err == nil {
		t.Fatal("separate pass assigned")
	}
	_, err = s.GetPipelinePolicyReviewAssignment(ctx, "two", first.CandidateID, "land-review")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross swarm history leaked: %v", err)
	}
}

func TestPolicyInlineReviewEvidenceRequiresCurrentReviewer(t *testing.T) {
	ctx := context.Background()
	s := inlineReviewStore(t, inlineReviewGraph, "reviewer")
	first := inlineApprove(t, s, 1)
	inlineObserveReview(t, s, "success", strings.Repeat("cd", 20), []string{})
	setActor(t, s, "reviewer", "incarnation=incarnation+1")
	if err := s.RecordPipelinePolicyReviewEvidence(ctx, "one", first.CandidateID, "land-review"); err == nil {
		t.Fatal("stale reviewer retained authority")
	}
	// Подтверждение человека не заменяет текущую review authority.
	result, err := s.ResolvePipelinePolicyActor(ctx, "one", 2)
	mustNil(t, err)
	if !result.Applied || !result.Rejected {
		t.Fatalf("revoked reviewer did not resolve terminally: %+v", result)
	}
}
