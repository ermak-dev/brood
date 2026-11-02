package store

import (
	"context"
	"errors"
	"testing"
)

// FEATURE: Workspace RPC reservations exclude task leases for the entire side-effect interval.
// SPEC: Acquisition is atomic; uncertain operations remain held until exact recovery.
func TestActorOperationCrashRecoveryRetainsReservation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	claim := coordinationFixture(t, s)
	if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET phase='running',operation='start',stage='done'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireActorOperation(ctx, claim.Target.UID, claim.Target.Generation, "crashed", "actor:source:1", "restart"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverActorOperations(ctx); err != nil {
		t.Fatal(err)
	}
	op, err := s.CurrentActorOperation(ctx, claim.Target.UID)
	if err != nil || op.State != "unconfirmed" {
		t.Fatalf("startup lost reservation: %+v %v", op, err)
	}
}

func TestActorOperationExcludesDispatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	claim := coordinationFixture(t, s)
	if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET phase='running',operation='start',stage='done'`); err != nil {
		t.Fatal(err)
	}
	op, err := s.AcquireActorOperation(ctx, claim.Target.UID, claim.Target.Generation, "operation-1", "actor:source:1", "restart")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.GetManagedActor(ctx, claim.Target.UID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginManagedActorOperation(ctx, actor.UID, actor.Revision, NewID(), "start"); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("start bypassed workspace reservation: %v", err)
	}
	if _, err := s.ClaimDispatchLease(ctx, claim); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("dispatch raced workspace operation: %v", err)
	}
	if err := s.FinishActorOperation(ctx, op, false); err != nil {
		t.Fatal(err)
	}
	held, err := s.CurrentActorOperation(ctx, claim.Target.UID)
	if err != nil || held.State != "unconfirmed" {
		t.Fatalf("lost uncertain reservation: %+v %v", held, err)
	}
	if _, err := s.AcquireActorOperation(ctx, claim.Target.UID, claim.Target.Generation, "operation-2", "actor:source:1", "restart"); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("uncertain operation silently replaced: %v", err)
	}
	if err := s.FinishActorOperation(ctx, held, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimDispatchLease(ctx, claim); err != nil {
		t.Fatalf("released reservation stayed blocked: %v", err)
	}
	if _, err := s.AcquireActorOperation(ctx, claim.Target.UID, claim.Target.Generation, "operation-3", "actor:source:1", "restart"); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("workspace raced active dispatch: %v", err)
	}
}
