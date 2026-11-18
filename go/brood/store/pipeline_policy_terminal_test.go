package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/workflowpolicy"
)

func policyTerminalResult(t *testing.T, outcome string, rules []workflowpolicy.Rule) (*Store, PipelineResultObservation, attest.ResultAttestation) {
	t.Helper()
	ctx := context.Background()
	graph := strings.Replace(startBuildFirst, "role: build,", "role: build, actionClasses: [work],", 1)
	graph = strings.Replace(graph, "{from: build, to: done}", "{from: build, to: done, condition: outcome=fail}", 1)
	if outcome == "fail" {
		graph = strings.Replace(graph, "condition: outcome=fail", "condition: outcome=success", 1)
	}
	s := policyPreviewStore(t, graph, rules)
	attempt := deliverOne(t, s, "terminal-result")
	in := ActorCompletion{SwarmID: "one", Sequence: 1, RequestID: attempt.RequestID, ActorUID: attempt.Actor.UID, Incarnation: attempt.Actor.Incarnation, Outcome: outcome, Report: "Retain the exact terminal report."}
	receipt, err := s.AcceptPipelineCompletion(ctx, in, completionNonce(), allow)
	mustNil(t, err)
	_, err = s.AcknowledgePipelineCompletion(ctx, receiptAck(receipt), allow)
	mustNil(t, err)
	intent := receipt.Intent
	request := attest.ResultRequest{Claim: attest.Claim{ActorUID: intent.ActorUID, Generation: intent.Incarnation, Swarm: intent.SwarmID, Step: intent.RequestID, Origin: intent.Origin, Commit: intent.BaseCommit, Challenge: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}, Pass: intent.Pass, Sequence: intent.Sequence}
	observation, err := s.ReservePipelineResultObservation(ctx, "one", 1, "terminal-observe", "swarm:one", request)
	mustNil(t, err)
	return s, observation, signedResult(t, request)
}

func TestTerminalPolicyPreviewRetainsNoEdgeAndVerifiedArtifact(t *testing.T) {
	for _, outcome := range []string{"success", "fail"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			s, observation, signed := policyTerminalResult(t, outcome, policyRules())
			mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
			before := dumpTables(t, s, executionTables...)
			preview, err := s.PreviewActorResultPolicy(ctx, "one", 1)
			mustNil(t, err)
			if preview.Transition != (pipeline.Transition{Terminal: true, Execution: "failed"}) || preview.Input.TargetKind != "exit" || preview.Input.Initial || preview.EvidenceID != observation.ID || preview.Input.Evidence.Artifact != signed.Head || !reflect.DeepEqual(preview.Input.Evidence.Paths, signed.Paths) || preview.Decision.State != workflowpolicy.WaitingHuman {
				t.Fatalf("terminal result lost policy/evidence/no-edge semantics: %+v", preview)
			}
			if stepOf(t, s, 1).Outcome != nil || stepCount(t, s) != 1 || dumpTables(t, s, executionTables...) != before {
				t.Fatal("terminal preview finalized the source or invented a successor")
			}
		})
	}
}

func TestTerminalPolicyResolutionWaitsForExactAnswerAndNeverInventsExit(t *testing.T) {
	for _, outcome := range []string{"success", "fail"} {
		for _, answer := range []string{"yes", "no"} {
			t.Run(outcome+"/"+answer, func(t *testing.T) {
				ctx := context.Background()
				s, observation, signed := policyTerminalResult(t, outcome, policyRules())
				mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
				first, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
				mustNil(t, err)
				if first.Applied || first.Decision.State != workflowpolicy.WaitingHuman || stepOf(t, s, 1).Outcome != nil {
					t.Fatalf("terminal source finalized before policy answer: %+v", first)
				}
				candidate, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "result")
				mustNil(t, err)
				_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", answer), allow)
				mustNil(t, err)
				result, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
				mustNil(t, err)
				w, err := s.GetSwarm(ctx, "one")
				mustNil(t, err)
				step := stepOf(t, s, 1)
				if !result.Applied || result.Rejected != (answer == "no") || w.Execution != "failed" || stepCount(t, s) != 1 || step.State != "completed" || step.Outcome == nil || *step.Outcome != outcome || step.Report != observation.Intent.Report || step.FinishedAt == nil || *step.FinishedAt == "" {
					t.Fatalf("terminal resolution rewrote source or invented exit: result=%+v step=%+v execution=%s", result, step, w.Execution)
				}
				history := append(executionTables, "pipeline_completion_intents", "pipeline_result_evidence", "pipeline_policy_candidates", "pipeline_policy_confirmations")
				before := dumpTables(t, s, history...)
				retry, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
				mustNil(t, err)
				if retry.Applied || dumpTables(t, s, history...) != before {
					t.Fatal("terminal replay changed immutable proof, source, notifications or graph")
				}
			})
		}
	}
}

func TestTerminalPolicyQuestionExplainsFinalizationWithoutSuccessor(t *testing.T) {
	ctx := context.Background()
	s, observation, signed := policyTerminalResult(t, "fail", policyRules())
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	result, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
	mustNil(t, err)
	request, err := s.GetSwarmRequest(ctx, "one", "policy:"+result.CandidateID)
	mustNil(t, err)
	var material struct {
		Target   string `json:"target"`
		Terminal bool   `json:"terminal"`
		Artifact string `json:"artifact"`
		Details  string `json:"details"`
	}
	mustNil(t, json.Unmarshal([]byte(request.Context), &material))
	if request.Question != "Finalize the reported result without a successor?" || !material.Terminal || material.Target != "" || material.Artifact != signed.Head || !strings.Contains(material.Details, "no edge matches") {
		t.Fatalf("terminal question disguises failure as an ordinary transition: %+v, %+v", request, material)
	}
}

func TestTerminalPolicyUnrelatedPathsFinishWithoutHumanRequest(t *testing.T) {
	ctx := context.Background()
	s, observation, signed := policyTerminalResult(t, "fail", policyRules())
	signed.Paths = []string{"docs/MODEL.md"}
	var err error
	signed.Signature, err = attest.SignResult(attest.ResultKey(resultTestToken), signed.ResultClaim)
	mustNil(t, err)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	result, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
	mustNil(t, err)
	requests, err := s.SwarmRequests(ctx, "one")
	mustNil(t, err)
	if !result.Applied || result.Rejected || result.Decision.State != workflowpolicy.Allowed || len(requests) != 0 || stepCount(t, s) != 1 {
		t.Fatalf("unrelated terminal evidence created a gate/exit or remained pending: %+v", result)
	}
}

func TestTerminalPolicyCannotReplaceReviewOrUnverifiedEvidenceWithFailure(t *testing.T) {
	ctx := context.Background()
	rules := []workflowpolicy.Rule{{ID: "review", Paths: []string{"go/**"}, Require: workflowpolicy.IndependentReview, ReviewRole: "review"}}
	s, observation, signed := policyTerminalResult(t, "fail", rules)
	if _, err := s.ResolvePipelinePolicyExit(ctx, "one", 1); err == nil {
		t.Fatal("terminal failure bypassed missing result evidence")
	}
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	result, err := s.ResolvePipelinePolicyExit(ctx, "one", 1)
	mustNil(t, err)
	if !result.Applied || !result.Rejected || result.Decision.State != workflowpolicy.Denied || stepOf(t, s, 1).Outcome == nil || *stepOf(t, s, 1).Outcome != "fail" {
		t.Fatal("missing next reviewer did not fail terminally while preserving actual failure")
	}
	before := dumpTables(t, s, executionTables...)
	if retry, err := s.ResolvePipelinePolicyExit(ctx, "one", 1); err == nil && retry.Applied {
		t.Fatal("terminal result resumed a failed source")
	}
	if before != dumpTables(t, s, executionTables...) {
		t.Fatal("terminal replay changed failed history")
	}
	stopped, _, _ := policyTerminalResult(t, "fail", rules)
	stopOne(t, stopped)
	if _, err := stopped.ResolvePipelinePolicyExit(ctx, "one", 1); err == nil {
		t.Fatal("terminal result resumed a stopped source")
	}
}
