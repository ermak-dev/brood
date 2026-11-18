package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// FEATURE: Registered intent и pre-registration failure занимают ровно два слота.
// SPEC: Immutable intent не удваивает registered child; failed slot не переиспользуется.
func TestEphemeralFailureAlongsideRegisteredEphemeralUsesExactTotal(t *testing.T) {
	ctx := context.Background()
	s, registeredJob := allocationFixture(t, 2)
	failedJob, err := s.EnqueueEphemeralChild(ctx, registeredJob.Parent, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "never-registered", "Rejected job", "pool", childAllow)
	mustNil(t, err)
	addEphemeralCapacity(t, s)
	_, err = s.AllocateEphemeralChild(ctx, registeredJob.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
	setActor(t, s, "isolated", "phase='running',stage='done',operation='start'")
	attestForRegistration(t, s, registeredJob)
	child, err := s.RegisterEphemeralChild(ctx, registeredJob.ID, childAllow)
	mustNil(t, err)
	mustNil(t, recordUnregisteredFailure(s, ctx, failedJob.ID, "Server rejected this unregistered obligation", childAllow))
	j, err := s.GetSwarmChildJoin(ctx, registeredJob.Parent, childAllow)
	mustNil(t, err)
	if len(j.Children) != 1 || len(j.UnregisteredFailures) != 1 || j.Evaluation.Open != 1 || j.Evaluation.State != "ready_fail" {
		t.Fatalf("registration/failure counted incorrectly: %+v", j)
	}
	if j.UnregisteredFailures[0].IntentID != failedJob.ID {
		t.Fatal("snapshot failure matched wrong job")
	}
	if _, err := s.EnqueueEphemeralChild(ctx, registeredJob.Parent, j.Policy, "third-job", "Overflow", "pool", childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("Total slot reused: %v", err)
	}
	in := ActorDeliveryInput{SwarmID: child.Executor.SwarmID, SourceKind: "child", SourceID: child.ID, RequestID: registeredJob.RequestID, Commit: registeredJob.SourceCommit, ActorUID: child.Executor.ActorUID, Incarnation: child.Executor.Incarnation, AtespaceUID: child.Executor.AtespaceUID, JournalID: strings.Repeat("a", 64), Payload: `{"to":"isolated","body":"instruction","waitSec":0}`}
	_, err = s.PrepareEphemeralChildDelivery(ctx, registeredJob.ID, in, childAllow)
	mustNil(t, err)
	_, err = s.CompleteSwarmChild(ctx, registeredJob.Parent, child.RequestID, child.Executor, child.Revision+1, "success", "Independent registered report", childAllow)
	mustNil(t, err)
	j, err = s.JoinSwarmChildren(ctx, registeredJob.Parent, childAllow)
	mustNil(t, err)
	if j.Evaluation.Open != 0 || j.Evaluation.State != "ready_fail" {
		t.Fatalf("mixed ephemeral group failed to settle: %+v", j)
	}
	// Seal настоящего child разрешён; pre-registration failure не становится report.
	mustNil(t, s.SealEphemeralChildResult(ctx, registeredJob.ID, childAllow))
	if err := s.SealEphemeralChildResult(ctx, failedJob.ID, childAllow); err == nil {
		t.Fatal("pre-proof failure obtained independent result seal")
	}
	if _, err := s.PrepareEphemeralChildDelivery(ctx, failedJob.ID, in, childAllow); err == nil {
		t.Fatal("failed job began instruction work")
	}
}
