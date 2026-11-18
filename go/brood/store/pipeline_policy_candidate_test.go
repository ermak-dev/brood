package store

// Сохранённый кандидат — immutable история вычисления, не право dispatch.
// Повтор не меняет identity/decision и не продвигает граф.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/workflowpolicy"
)

func TestPolicyCandidateRetainsExactInitialDecisionWithoutProgression(t *testing.T) {
	ctx := context.Background()
	graph := strings.Replace(startBuildFirst, "role: build,", "role: build, actionClasses: [work],", 1)
	s := policyPreviewStore(t, graph, policyRules())
	before := dumpTables(t, s, executionTables...)
	first, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "entry", "candidate-1")
	mustNil(t, err)
	if first.ID != "candidate-1" || first.Version != 1 || first.CreatedAt == "" || first.Boundary != "entry" || first.Preview.Policy.Revision != 1 || first.Preview.Decision.State != workflowpolicy.Allowed {
		t.Fatalf("incomplete policy candidate: %+v", first)
	}
	_, err = s.SetProjectWorkflowPolicy(ctx, "root", "one", 1, []workflowpolicy.Rule{{ID: "deny", Actions: []string{"work"}, Require: workflowpolicy.Deny}}, allow)
	mustNil(t, err)
	again, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "entry", "different-id")
	mustNil(t, err)
	if !reflect.DeepEqual(again, first) {
		t.Fatalf("retry rewrote the original candidate: %+v", again)
	}
	if dumpTables(t, s, executionTables...) != before {
		t.Fatal("recording a candidate applied it or opened a policy request")
	}
	for _, query := range []string{
		`UPDATE pipeline_policy_candidates SET preview=preview`,
		`DELETE FROM pipeline_policy_candidates`,
		`INSERT OR REPLACE INTO pipeline_policy_candidates SELECT * FROM pipeline_policy_candidates`,
	} {
		if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "policy candidate history is immutable") {
			t.Fatalf("candidate history fence: %v", err)
		}
	}
	stopOne(t, s)
	retained, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "entry")
	mustNil(t, err)
	if !reflect.DeepEqual(retained, first) {
		t.Fatal("stop erased or reinterpreted historical decision")
	}
	if _, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "entry", "late-retry"); err == nil {
		t.Fatal("historical allowed decision was re-recorded as current permission")
	}
}

func TestPolicyCandidateResultRequiresObservedEvidenceAndRetainsObligations(t *testing.T) {
	ctx := context.Background()
	s, observation, signed := policyPreviewResult(t)
	if _, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "result", "early"); !errors.Is(err, ErrPipelineCompletionPending) {
		t.Fatalf("unobserved result became a durable decision: %v", err)
	}
	if _, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "result"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed evaluation wrote history: %v", err)
	}
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	candidate, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "result", "held-result")
	mustNil(t, err)
	if candidate.Preview.EvidenceID != observation.ID || candidate.Preview.Input.Initial || candidate.Preview.Decision.State != workflowpolicy.WaitingHuman || !reflect.DeepEqual(candidate.Preview.Decision.Human, []string{"storage"}) {
		t.Fatalf("durable candidate lost exact artifact/obligation: %+v", candidate)
	}
	if stepCount(t, s) != 1 || stepOf(t, s, 1).Outcome != nil {
		t.Fatal("recording the candidate finalized or advanced work")
	}
}

func TestPolicyCandidateSchemaRejectsMissingDecision(t *testing.T) {
	ctx := context.Background()
	s := policyPreviewStore(t, startBuildFirst, policyRules())
	preview, err := s.PreviewInitialPipelinePolicy(ctx, "one", 1)
	mustNil(t, err)
	raw, err := json.Marshal(preview)
	mustNil(t, err)
	var document map[string]json.RawMessage
	mustNil(t, json.Unmarshal(raw, &document))
	delete(document, "Decision")
	raw, err = json.Marshal(document)
	mustNil(t, err)
	_, err = s.db.Exec(`INSERT INTO pipeline_policy_candidates VALUES('invalid',1,'one',1,1,'entry',1,NULL,?,'now')`, string(raw))
	if err == nil {
		t.Fatal("missing decision passed SQL CHECK as NULL")
	}
}

func TestPolicyCandidateSchemaBindsTheExactPassPolicy(t *testing.T) {
	for _, field := range []string{"swarm", "rules", "legacy"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			s := policyPreviewStore(t, startBuildFirst, policyRules())
			preview, err := s.PreviewInitialPipelinePolicy(ctx, "one", 1)
			mustNil(t, err)
			switch field {
			case "swarm":
				preview.Policy.SwarmID = "foreign"
			case "rules":
				preview.Policy.Rules[0].Paths[0] = "docs/**"
			case "legacy":
				preview.Policy.LegacyEmpty = true
			}
			raw, err := json.Marshal(preview)
			mustNil(t, err)
			_, err = s.db.Exec(`INSERT INTO pipeline_policy_candidates VALUES('mismatch',1,'one',1,1,'entry',1,NULL,?,'now')`, string(raw))
			if err == nil {
				t.Fatalf("candidate accepted mismatched pass policy %s", field)
			}
		})
	}
}

func TestPolicyCandidateSchemaRejectsNullIdentity(t *testing.T) {
	ctx := context.Background()
	s := policyPreviewStore(t, startBuildFirst, policyRules())
	preview, err := s.PreviewInitialPipelinePolicy(ctx, "one", 1)
	mustNil(t, err)
	raw, err := json.Marshal(preview)
	mustNil(t, err)
	_, err = s.db.Exec(`INSERT INTO pipeline_policy_candidates VALUES(NULL,1,'one',1,1,'entry',1,NULL,?,'now')`, string(raw))
	if err == nil {
		t.Fatal("NULL primary key created unreadable policy history")
	}
}

func TestPolicyCandidateSnapshotBindingPreservesEscapedPaths(t *testing.T) {
	ctx := context.Background()
	rules := []workflowpolicy.Rule{{ID: "paths", Paths: []string{"web/<svg>/**", "docs/цель/**", "files/\"quoted\"/**"}, Require: workflowpolicy.Human}}
	s := policyPreviewStore(t, startBuildFirst, rules)
	candidate, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "entry", "escaped-paths")
	mustNil(t, err)
	if !reflect.DeepEqual(candidate.Preview.Policy.Rules, rules) {
		t.Fatal("SQL snapshot binding normalized or lost literal policy paths")
	}
}

func TestPolicyCandidateConcurrentWritersKeepOneIdentity(t *testing.T) {
	ctx := context.Background()
	s := policyPreviewStore(t, startBuildFirst, policyRules())
	var wg sync.WaitGroup
	results := make(chan PipelinePolicyCandidate, 6)
	for range 6 {
		wg.Go(func() {
			candidate, err := s.RecordPipelinePolicyCandidate(ctx, "one", 1, "entry", NewID())
			if err != nil {
				t.Error(err)
				return
			}
			results <- candidate
		})
	}
	wg.Wait()
	close(results)
	var first PipelinePolicyCandidate
	count := 0
	for candidate := range results {
		if count == 0 {
			first = candidate
		} else if !reflect.DeepEqual(first, candidate) {
			t.Fatal("concurrent writer replaced candidate identity")
		}
		count++
	}
	var rows int
	mustNil(t, s.db.QueryRow(`SELECT COUNT(*) FROM pipeline_policy_candidates`).Scan(&rows))
	if count != 6 || rows != 1 {
		t.Fatalf("concurrent results=%d rows=%d", count, rows)
	}
}
