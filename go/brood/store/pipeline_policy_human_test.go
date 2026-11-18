package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/workflowpolicy"
)

const policyHumanGraph = `apiVersion: brood.dev/v1alpha1
name: build-first
start: build
exit: done
nodes:
  build: {type: actor, role: build, actionClasses: [work], prompt: work}
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

func policyHumanResult(t *testing.T, requirement string) (*Store, PipelineResultObservation, attest.ResultAttestation) {
	t.Helper()
	rule := workflowpolicy.Rule{ID: "storage", Paths: []string{"go/**"}, Require: requirement}
	if requirement == workflowpolicy.IndependentReview {
		rule.ReviewRole = "review"
	}
	s := policyPreviewStore(t, policyHumanGraph, []workflowpolicy.Rule{rule})
	_, observation, signed := policyPreviewObserve(t, s)
	mustNil(t, s.RecordPipelineResultEvidence(context.Background(), observation, attest.ResultKey(resultTestToken), signed))
	return s, observation, signed
}

func TestPolicyHumanCandidateDoesNotOpenGraphQuestionBeforeExactConfirmation(t *testing.T) {
	ctx := context.Background()
	s, observation, _ := policyHumanResult(t, workflowpolicy.Human)
	first, err := s.ResolvePipelinePolicyHuman(ctx, "one", 1)
	mustNil(t, err)
	if first.Applied || first.Decision.State != workflowpolicy.WaitingHuman || stepCount(t, s) != 1 {
		t.Fatalf("human boundary bypassed policy: %+v", first)
	}
	requests, err := s.SwarmRequests(ctx, "one")
	mustNil(t, err)
	if len(requests) != 1 || requests[0].Origin != "policy" {
		t.Fatal("graph question opened before policy release")
	}
	candidate, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "result")
	mustNil(t, err)
	if candidate.Preview.Input.TargetKind != "human" || candidate.Preview.Transition.Next != "ask" {
		t.Fatal("candidate is not exact human successor")
	}
	_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "yes"), allow)
	mustNil(t, err)
	second, err := s.ResolvePipelinePolicyHuman(ctx, "one", 1)
	mustNil(t, err)
	if !second.Applied || second.Rejected || second.Decision.State != workflowpolicy.Allowed || stepCount(t, s) != 2 {
		t.Fatalf("exact human confirmation failed: %+v", second)
	}
	source := stepOf(t, s, 1)
	if source.State != "completed" || source.Outcome == nil || *source.Outcome != observation.Intent.Outcome || source.Report != observation.Intent.Report {
		t.Fatal("human gate rewrote actor outcome/report")
	}
	next := stepOf(t, s, 2)
	if next.NodeID != "ask" || next.State != "waiting" || next.Pass != source.Pass {
		t.Fatal("human question was not same-pass successor")
	}
	requests, err = s.SwarmRequests(ctx, "one")
	mustNil(t, err)
	var nodeQuestions int
	for _, request := range requests {
		if request.Origin == "node" {
			nodeQuestions++
			if request.State != "open" || request.Sequence == nil || *request.Sequence != 2 || request.Context != stepReportContext(1, "build", observation.Intent.Outcome, observation.Intent.Report) {
				t.Fatalf("human question lost actual report: %+v", request)
			}
		}
	}
	if nodeQuestions != 1 {
		t.Fatal("expected exactly one human question")
	}
	before := dumpTables(t, s, append(executionTables, "pipeline_completion_intents", "pipeline_policy_candidates", "pipeline_policy_confirmations")...)
	retry, err := s.ResolvePipelinePolicyHuman(ctx, "one", 1)
	mustNil(t, err)
	if retry.Applied || before != dumpTables(t, s, append(executionTables, "pipeline_completion_intents", "pipeline_policy_candidates", "pipeline_policy_confirmations")...) {
		t.Fatal("replay duplicated graph question/history")
	}
}

func TestPolicyHumanRejectionRetainsActualSuccessWithoutOpeningQuestion(t *testing.T) {
	for _, kind := range []string{"no", workflowpolicy.Deny, workflowpolicy.IndependentReview} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			requirement := kind
			if kind == "no" {
				requirement = workflowpolicy.Human
			}
			s, observation, signed := policyHumanResult(t, requirement)
			result, err := s.ResolvePipelinePolicyHuman(ctx, "one", 1)
			mustNil(t, err)
			if kind == "no" {
				candidate, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "result")
				mustNil(t, err)
				_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "no"), allow)
				mustNil(t, err)
				result, err = s.ResolvePipelinePolicyHuman(ctx, "one", 1)
				mustNil(t, err)
			}
			if stepCount(t, s) != 1 {
				t.Fatal("rejected/missing review opened a human question")
			}
			swarm, err := s.GetSwarm(ctx, "one")
			mustNil(t, err)
			step := stepOf(t, s, 1)
			if !result.Applied || !result.Rejected || swarm.Execution != "failed" || step.Outcome == nil || *step.Outcome != "success" || step.Report != observation.Intent.Report {
				t.Fatal("policy rejection invented actor failure")
			}
			evidence, err := s.GetPipelineResultEvidence(ctx, "one", 1)
			mustNil(t, err)
			if !reflect.DeepEqual(evidence, signed) {
				t.Fatal("human rejection erased signed evidence")
			}
		})
	}
}

func TestPolicyHumanUnrelatedDiffAdvancesAndStoppedSourceCannot(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated", true: "stopped"}[stopped], func(t *testing.T) {
			ctx := context.Background()
			s := policyPreviewStore(t, policyHumanGraph, policyRules())
			_, observation, signed := policyPreviewObserve(t, s)
			signed.Paths = []string{"docs/readme.md"}
			var err error
			signed.Signature, err = attest.SignResult(attest.ResultKey(resultTestToken), signed.ResultClaim)
			mustNil(t, err)
			mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
			if stopped {
				stopOne(t, s)
			}
			result, err := s.ResolvePipelinePolicyHuman(ctx, "one", 1)
			if stopped {
				if err == nil || result.Applied || stepCount(t, s) != 1 {
					t.Fatal("human policy resumed stopped source")
				}
				return
			}
			mustNil(t, err)
			if !result.Applied || result.Decision.State != workflowpolicy.Allowed || stepCount(t, s) != 2 {
				t.Fatal("unrelated verified diff failed to open human question")
			}
		})
	}
}

func TestPolicyHumanResolverDoesNotApplyActorOrExitCandidate(t *testing.T) {
	ctx := context.Background()
	s, observation, signed := policyPreviewResult(t)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	if _, err := s.ResolvePipelinePolicyHuman(ctx, "one", 1); !errors.Is(err, ErrNoPolicyHuman) {
		t.Fatalf("human resolver took an exit: %v", err)
	}
	graph := strings.Replace(policyHumanGraph, "type: human\n    question: Publish this artifact?\n    options:\n      - {label: Publish, outcome: success}", "type: actor\n    role: build\n    actionClasses: [landing]\n    prompt: promote", 1)
	actorStore := policyPreviewStore(t, graph, policyRules())
	_, observation, signed = policyPreviewObserve(t, actorStore)
	mustNil(t, actorStore.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	if _, err := actorStore.ResolvePipelinePolicyHuman(ctx, "one", 1); !errors.Is(err, ErrNoPolicyHuman) {
		t.Fatalf("human resolver took an actor: %v", err)
	}
}
