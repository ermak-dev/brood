package store

// BUG: Вмешательство теряло pending-шаг и скрывало причину отказа завершения.
// HYPOTHESIS: До резерва назначение задаёт роль; после прерывания attestation не разрешает completion.

import (
	"context"
	"errors"
	"testing"
)

func TestReviewOwnerInterventionIncludesPendingBoundStep(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	a, err := s.GetManagedActor(ctx, "a")
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	before := stepOf(t, s, 1)
	if before.State != "pending" || before.ActorUID != "" {
		t.Fatalf("unexpected pending fixture: %+v", before)
	}
	op, err := s.AcquireOwnerActorOperation(ctx, a, "pending-owner-control", "operator:"+sw.OwnerID, "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	after := stepOf(t, s, 1)
	if after.State != "unconfirmed" {
		t.Errorf("bound pending step not interrupted by owner intervention: %+v", after)
	}
	attempt, err := s.ReserveActorStep(ctx, "one", "automatic-after-owner", "swarm:one")
	if err == nil {
		t.Errorf("automatic workspace attestation admitted after acknowledged owner intervention: %+v", attempt)
	}
}

func TestReviewInterruptedOrdinaryCompletionReturnsTypedConflict(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	attempt := attestOne(t, s, "ordinary-owner-interrupt")
	mustNil(t, s.FinishActorOperation(ctx, attempt.Lease, true))
	a, err := s.GetManagedActor(ctx, attempt.Actor.UID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	op, err := s.AcquireOwnerActorOperation(ctx, a, "ordinary-owner-control", "operator:"+sw.OwnerID, "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	err = s.CompletePipelineActorVisit(ctx, "one", "one", 1, attempt.RequestID, a.UID, a.Incarnation, "success", "late ordinary evidence")
	if !errors.Is(err, ErrPipelineVisitInterrupted) {
		t.Errorf("interrupted completion returns opaque storage error instead of typed public conflict: %v", err)
	}
}
