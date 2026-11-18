package store

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/workflowpolicy"
)

func TestPolicyExitAppliesOnlyAfterExactHumanAnswerAndPreservesActualOutcome(t *testing.T) {
	ctx := context.Background()
	s, observation, signed := policyPreviewResult(t)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	first, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
	mustNil(t, err)
	if first.Applied || first.Decision.State != workflowpolicy.WaitingHuman || stepCount(t, s) != 1 {
		t.Fatalf("protected artifact exited without confirmation: %+v", first)
	}
	candidate, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "result")
	mustNil(t, err)
	_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "yes"), allow)
	mustNil(t, err)
	second, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
	mustNil(t, err)
	if !second.Applied || second.Decision.State != workflowpolicy.Allowed || stepCount(t, s) != 2 {
		t.Fatalf("exact confirmation failed to apply one exit: %+v", second)
	}
	step := stepOf(t, s, 1)
	if step.State != "completed" || step.Outcome == nil || *step.Outcome != observation.Intent.Outcome || step.Report != observation.Intent.Report {
		t.Fatalf("policy permission rewrote actual work: %+v", step)
	}
	before := dumpTables(t, s, append(executionTables, "pipeline_completion_intents", "pipeline_policy_candidates", "pipeline_policy_confirmations")...)
	retry, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
	mustNil(t, err)
	if retry.Applied || dumpTables(t, s, append(executionTables, "pipeline_completion_intents", "pipeline_policy_candidates", "pipeline_policy_confirmations")...) != before {
		t.Fatal("replayed exit repeated application/history/notification")
	}
}

func TestPolicyExitRejectionFailsExecutionWithoutInventingActorFailure(t *testing.T) {
	ctx := context.Background()
	s, observation, signed := policyPreviewResult(t)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	_, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
	mustNil(t, err)
	candidate, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "result")
	mustNil(t, err)
	_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "no"), allow)
	mustNil(t, err)
	result, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
	mustNil(t, err)
	if !result.Applied || !result.Rejected || stepCount(t, s) != 1 {
		t.Fatalf("rejection created a successor: %+v", result)
	}
	swarm, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	step := stepOf(t, s, 1)
	if swarm.Execution != "failed" || step.Outcome == nil || *step.Outcome != "success" || step.Report != observation.Intent.Report {
		t.Fatal("policy rejection was misreported as actor failure")
	}
	evidence, err := s.GetPipelineResultEvidence(ctx, "one", 1)
	mustNil(t, err)
	if !reflect.DeepEqual(evidence, signed) {
		t.Fatal("policy rejection erased artifact evidence")
	}
}

func TestPolicyExitDenyAndMissingIndependentReviewDoNotBecomePermission(t *testing.T) {
	for _, requirement := range []string{workflowpolicy.Deny, workflowpolicy.IndependentReview} {
		t.Run(requirement, func(t *testing.T) {
			ctx := context.Background()
			graph := strings.Replace(startBuildFirst, "role: build,", "role: build, actionClasses: [work],", 1)
			rule := workflowpolicy.Rule{ID: "protected", Paths: []string{"go/**"}, Require: requirement}
			if requirement == workflowpolicy.IndependentReview {
				rule.ReviewRole = "review"
			}
			s := policyPreviewStore(t, graph, []workflowpolicy.Rule{rule})
			_, observation, signed := policyPreviewObserve(t, s)
			mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
			result, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
			mustNil(t, err)
			if requirement == workflowpolicy.Deny {
				if !result.Applied || !result.Rejected || stepOf(t, s, 1).Outcome == nil || *stepOf(t, s, 1).Outcome != "success" {
					t.Fatal("deny did not retain successful actual work independently")
				}
			} else if !result.Applied || !result.Rejected || result.Decision.State != workflowpolicy.Denied || stepOf(t, s, 1).Outcome == nil || *stepOf(t, s, 1).Outcome != "success" {
				t.Fatal("unavailable graph reviewer did not terminally reject while retaining actual work")
			}
		})
	}
}

func TestPolicyExitUnrelatedVerifiedPathsAdvanceWithoutHumanRequest(t *testing.T) {
	ctx := context.Background()
	s, observation, signed := policyPreviewResult(t)
	signed.Paths = []string{"docs/readme.md"}
	var err error
	signed.Signature, err = attest.SignResult(attest.ResultKey(resultTestToken), signed.ResultClaim)
	mustNil(t, err)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	result, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
	mustNil(t, err)
	requests, err := s.SwarmRequests(ctx, "one")
	mustNil(t, err)
	if !result.Applied || result.Rejected || result.Decision.State != workflowpolicy.Allowed || len(requests) != 0 || stepCount(t, s) != 2 {
		t.Fatal("unrelated verified artifact required approval or failed to exit")
	}
}

func TestPolicyExitCannotCrossStopOrUnverifiedEvidence(t *testing.T) {
	ctx := context.Background()
	s, observation, signed := policyPreviewResult(t)
	if _, err := s.ResolvePipelinePolicyExit(ctx, "one", 1); err == nil {
		t.Fatal("unobserved artifact completed the graph")
	}
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	stopOne(t, s)
	if _, err := s.ResolvePipelinePolicyExit(ctx, "one", 1); err == nil {
		t.Fatal("policy resolver resumed a stopped source")
	}
}
