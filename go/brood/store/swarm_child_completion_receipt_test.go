package store

// FEATURE: Серверный child_join проходит тот же receipt/evidence контракт (#290).
// SPEC: Только computed aggregate, без обхода через старый JoinSwarmChildren;
// все дети завершены до acceptance, до ACK нельзя резервировать root observation.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
)

func policyChildFixture(t *testing.T) (*Store, SwarmChildParent, SwarmChildPrincipal) {
	t.Helper()
	ctx := context.Background()
	s := goalStore(t)
	_, err := s.ReserveManagedActor(ctx, managedRequest("b", "one", 202))
	mustNil(t, err)
	setActor(t, s, "b", "phase='running', stage='done', operation='start'")
	graph := strings.Replace(startBuildFirst, "role: build,", "role: build, actionClasses: [work],", 1)
	publish(t, s, "one", map[string]string{"build-first": graph})
	_, err = s.SetProjectWorkflowPolicy(ctx, "root", "one", 0, policyRules(), allow)
	mustNil(t, err)
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "test", ActorUID: "b"}))
	mustStart(t, s, "build-first")
	attempt := attestOne(t, s, "parent-policy")
	mustNil(t, s.FinishActorOperation(ctx, attempt.Lease, true))
	parent := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: attempt.Lease.ProjectID, SwarmID: attempt.SwarmID, AtespaceUID: attempt.Actor.AtespaceUID, ActorUID: attempt.Actor.UID, Incarnation: attempt.Actor.Incarnation}, Pass: attempt.Pass, Sequence: attempt.Sequence, RequestID: attempt.RequestID}
	executor := parent.Principal
	executor.ActorUID = "b"
	_, err = s.CreateSwarmChildJoin(ctx, parent, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, childAllow)
	mustNil(t, err)
	return s, parent, executor
}

func TestPolicyChildJoinAcceptsOnlyComputedCompletionWithoutAdvancing(t *testing.T) {
	ctx := context.Background()
	s, parent, executor := policyChildFixture(t)
	for _, tc := range []struct{ key, outcome string }{{"first", "success"}, {"second", "fail"}} {
		child := engineChild(t, s, parent, tc.key)
		child, err := s.StartSwarmChild(ctx, parent, tc.key, executor, child.Revision, childAllow)
		mustNil(t, err)
		_, err = s.CompleteSwarmChild(ctx, parent, tc.key, executor, child.Revision, tc.outcome, "Child report "+tc.key, childAllow)
		mustNil(t, err)
	}
	if _, err := s.JoinSwarmChildren(ctx, parent, childAllow); !errors.Is(err, ErrPipelineCompletionPending) {
		t.Fatalf("old join bypassed policy: %v", err)
	}
	joined, err := s.AcceptSwarmChildJoin(ctx, parent, completionNonce(), childAllow)
	mustNil(t, err)
	if joined.CompletionReceipt == nil {
		t.Fatal("settled join has no completion receipt")
	}
	receipt := *joined.CompletionReceipt
	if receipt.Intent.Outcome != "fail" || receipt.Intent.Report != ChildJoinReport(joined) {
		t.Fatal("join acceptance trusted actor outcome/report instead of computed aggregate")
	}
	step, err := s.GetSwarmStep(ctx, "one", parent.Sequence)
	mustNil(t, err)
	if step.Outcome != nil || step.State == "completed" {
		t.Fatal("join advanced before result evidence and policy")
	}
	request := attest.ResultRequest{Claim: attest.Claim{ActorUID: parent.Principal.ActorUID, Generation: parent.Principal.Incarnation, Swarm: parent.Principal.SwarmID, Step: parent.RequestID, Origin: receipt.Intent.Origin, Commit: receipt.Intent.BaseCommit, Challenge: completionNonce()}, Pass: parent.Pass, Sequence: parent.Sequence}
	if _, err := s.ReservePipelineResultObservation(ctx, "one", parent.Sequence, "before-receipt", "caller", request); !errors.Is(err, ErrPipelineCompletionReceiptPending) {
		t.Fatal("child join inferred acceptance receipt")
	}
	_, err = s.AcknowledgePipelineCompletion(ctx, receiptAck(receipt), allow)
	mustNil(t, err)
	if _, err := s.ReservePipelineResultObservation(ctx, "one", parent.Sequence, "after-receipt", "caller", request); err != nil {
		t.Fatalf("settled child group deadlocked its own result observation: %v", err)
	}
}

func TestPolicyChildJoinCannotAcceptWithOutstandingObligations(t *testing.T) {
	ctx := context.Background()
	s, parent, _ := policyChildFixture(t)
	engineChild(t, s, parent, "first")
	joined, err := s.AcceptSwarmChildJoin(ctx, parent, completionNonce(), childAllow)
	mustNil(t, err)
	if joined.CompletionReceipt != nil {
		t.Fatal("incomplete child membership was accepted as parent completion")
	}
	if _, err := s.GetPipelineCompletionIntent(ctx, "one", parent.Sequence); !errors.Is(err, ErrNotFound) {
		t.Fatal("incomplete join wrote completion intent")
	}
}
