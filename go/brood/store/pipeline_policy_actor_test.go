package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/workflowpolicy"
)

const policyActorGraph = `apiVersion: brood.dev/v1alpha1
name: build-first
start: build
exit: done
nodes:
  build: {type: actor, role: build, actionClasses: [work], prompt: work}
  land: {type: actor, role: build, actionClasses: [landing], prompt: promote}
  done: {type: exit}
edges:
  - {from: build, to: land}
  - {from: land, to: done}
goalGates: [done]
`

func TestPolicyActorSuccessorCannotMintDuplicateEntryCandidate(t *testing.T) {
	ctx := context.Background()
	s := policyPreviewStore(t, policyActorGraph, []workflowpolicy.Rule{{ID: "deploy", Actions: []string{"deploy"}, Require: workflowpolicy.Deny}})
	_, observation, signed := policyPreviewObserve(t, s)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	result, err := s.ResolvePipelinePolicyActor(ctx, "one", 1)
	mustNil(t, err)
	if !result.Applied {
		t.Fatal("allowed result did not apply")
	}
	_, err = s.RecordPipelinePolicyCandidate(ctx, "one", 2, "entry", "duplicate-entry")
	if !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("duplicate successor entry did not refuse with a readable Store conflict: %v", err)
	}
}

func TestPolicyHumanRoutingPreservesEvidenceAndRequiresSeparateApproval(t *testing.T) {
	ctx := context.Background()
	graph := strings.Replace(policyActorGraph, "  done: {type: exit}", "  ask:\n    type: human\n    question: Continue?\n    options:\n      - {label: Continue, outcome: success}\n  done: {type: exit}", 1)
	graph = strings.Replace(graph, "  - {from: build, to: land}", "  - {from: build, to: ask}\n  - {from: ask, to: land}", 1)
	s := policyPreviewStore(t, graph, policyRules())
	_, observation, signed := policyPreviewObserve(t, s)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	first, err := s.ResolvePipelinePolicyHuman(ctx, "one", 1)
	mustNil(t, err)
	_, err = s.AnswerPipelinePolicy(ctx, PipelinePolicyAnswer{SwarmID: "one", CandidateID: first.CandidateID, UserID: "u", ExpectedRevision: 1, Answer: "yes"}, allow)
	mustNil(t, err)
	first, err = s.ResolvePipelinePolicyHuman(ctx, "one", 1)
	mustNil(t, err)
	if !first.Applied {
		t.Fatal("source did not progress to question")
	}
	requests, err := s.SwarmRequests(ctx, "one")
	mustNil(t, err)
	for _, r := range requests {
		if r.Origin == "node" {
			_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: r.ID, UserID: "u", Outcome: "success"}, allow)
			mustNil(t, err)
		}
	}
	if _, err = s.ReserveActorStep(ctx, "one", "held-after-graph-yes", "swarm:one"); !errors.Is(err, ErrPipelinePolicyPending) {
		t.Fatalf("graph answer authorized protected landing: %v", err)
	}
	entry, err := s.GetPipelinePolicyCandidate(ctx, "one", 3, "entry")
	mustNil(t, err)
	if entry.ID == first.CandidateID || entry.Preview.Input.Initial || entry.Preview.EvidenceID != observation.ID || entry.Preview.Input.Evidence.Artifact != signed.Head || entry.Preview.Decision.State != workflowpolicy.WaitingHuman {
		t.Fatalf("later entry lost scope: %+v", entry)
	}
	if _, err := s.CurrentActorOperation(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal("held entry acquired lease")
	}
	_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(entry, "u", "yes"), allow)
	mustNil(t, err)
	attempt, err := s.ReserveActorStep(ctx, "one", "separately-approved", "swarm:one")
	mustNil(t, err)
	mustNil(t, s.CheckActorStepAttestation(ctx, attempt))
	stopOne(t, s)
	if err := s.CheckActorStepAttestation(ctx, attempt); err == nil {
		t.Fatal("approval ignored current owner stop")
	}
}

func TestPolicyActorResultApprovalIsReusedButNeverDispatchesBeforeApproval(t *testing.T) {
	ctx := context.Background()
	s := policyPreviewStore(t, policyActorGraph, policyRules())
	_, observation, signed := policyPreviewObserve(t, s)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	first, err := s.ResolvePipelinePolicyActor(ctx, "one", 1)
	mustNil(t, err)
	if first.Applied || first.Decision.State != workflowpolicy.WaitingHuman || stepCount(t, s) != 1 {
		t.Fatalf("unapproved successor applied: %+v", first)
	}
	candidate, err := s.GetPipelinePolicyCandidate(ctx, "one", 1, "result")
	mustNil(t, err)
	_, err = s.AnswerPipelinePolicy(ctx, policyAnswer(candidate, "u", "yes"), allow)
	mustNil(t, err)
	result, err := s.ResolvePipelinePolicyActor(ctx, "one", 1)
	mustNil(t, err)
	if !result.Applied {
		t.Fatal("approved actor successor did not progress")
	}
	replay, err := s.ResolvePipelinePolicyActor(ctx, "one", 1)
	mustNil(t, err)
	if replay.Applied || stepCount(t, s) != 2 {
		t.Fatal("result replay created another actor")
	}
	_, err = s.SetProjectWorkflowPolicy(ctx, "root", "one", 1, []workflowpolicy.Rule{{ID: "new-deny", Actions: []string{"landing"}, Require: workflowpolicy.Deny}}, allow)
	mustNil(t, err)
	attempt, err := s.ReserveActorStep(ctx, "one", "approved-successor", "swarm:one")
	mustNil(t, err)
	mustNil(t, s.CheckActorStepAttestation(ctx, attempt))
	if _, err := s.GetPipelinePolicyCandidate(ctx, "one", 2, "entry"); !errors.Is(err, ErrNotFound) {
		t.Fatal("successor requested duplicate policy permission")
	}
}

func TestPolicyInitialDenialIsTerminalAndReadableWithoutLease(t *testing.T) {
	ctx := context.Background()
	s := policyPreviewStore(t, policyActorGraph, []workflowpolicy.Rule{{ID: "stop-work", Actions: []string{"work"}, Require: workflowpolicy.Deny}})
	_, err := s.ReserveActorStep(ctx, "one", "denied", "swarm:one")
	if !errors.Is(err, ErrActorStepRefused) || !strings.Contains(err.Error(), "stop-work") {
		t.Fatalf("denial did not provide terminal reason: %v", err)
	}
	w, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	step := stepOf(t, s, 1)
	if w.Execution != "failed" || step.State != "refused" || step.Outcome != nil || step.RequestID != "" || !strings.Contains(step.Detail, "stop-work") {
		t.Fatalf("denial did not stop honestly: swarm=%+v step=%+v", w, step)
	}
	if _, err := s.CurrentActorOperation(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("denial acquired lease: %v", err)
	}
}

func TestPolicyActorSuccessorCanReserveFromExactAllowedResult(t *testing.T) {
	ctx := context.Background()
	s := policyPreviewStore(t, policyActorGraph, policyRules())
	_, observation, signed := policyPreviewObserve(t, s)
	signed.Paths = []string{"docs/readme.md"}
	var err error
	signed.Signature, err = attest.SignResult(attest.ResultKey(resultTestToken), signed.ResultClaim)
	mustNil(t, err)
	mustNil(t, s.RecordPipelineResultEvidence(ctx, observation, attest.ResultKey(resultTestToken), signed))
	result, err := s.ResolvePipelinePolicyActor(ctx, "one", 1)
	mustNil(t, err)
	if !result.Applied || result.Decision.State != workflowpolicy.Allowed {
		t.Fatalf("actor result not applied: %+v", result)
	}
	attempt, err := s.ReserveActorStep(ctx, "one", "successor", "swarm:one")
	mustNil(t, err)
	if attempt.Sequence != 2 || attempt.NodeID != "land" {
		t.Fatalf("wrong successor: %+v", attempt)
	}
	mustNil(t, s.CheckActorStepAttestation(ctx, attempt))
}

func TestPolicyHumanToActorNonmatchingRuleDoesNotHoldForever(t *testing.T) {
	ctx := context.Background()
	graph := strings.Replace(policyActorGraph, "start: build", "start: ask", 1)
	graph = strings.Replace(graph, "nodes:\n", "nodes:\n  ask:\n    type: human\n    question: Continue?\n    options:\n      - {label: Continue, outcome: success}\n", 1)
	graph = strings.Replace(graph, "edges:\n", "edges:\n  - {from: ask, to: build}\n", 1)
	s := policyPreviewStore(t, graph, []workflowpolicy.Rule{{ID: "land", Actions: []string{"landing"}, Require: workflowpolicy.Human}})
	requests, err := s.SwarmRequests(ctx, "one")
	mustNil(t, err)
	_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: requests[0].ID, UserID: "u", Outcome: "success"}, allow)
	mustNil(t, err)
	attempt, err := s.ReserveActorStep(ctx, "one", "after-question", "swarm:one")
	mustNil(t, err)
	if attempt.Sequence != 2 || attempt.NodeID != "build" {
		t.Fatalf("wrong successor: %+v", attempt)
	}
	mustNil(t, s.CheckActorStepAttestation(ctx, attempt))
}
