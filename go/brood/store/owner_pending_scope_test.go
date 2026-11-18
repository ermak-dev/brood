package store

import (
	"context"
	"testing"
)

func TestOwnerInterventionDoesNotInterruptAnotherPendingRole(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	other, err := s.GetManagedActor(ctx, "b")
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, "one")
	mustNil(t, err)
	op, err := s.AcquireOwnerActorOperation(ctx, other, "unrelated-owner-control", "operator:"+sw.OwnerID, "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	if step := stepOf(t, s, 1); step.State != "pending" {
		t.Fatalf("unrelated intervention changed pending step: %+v", step)
	}
	attempt, err := s.ReserveActorStep(ctx, "one", "assigned-actor-after-unrelated-owner", "swarm:one")
	mustNil(t, err)
	if attempt.Actor.UID != "a" {
		t.Fatalf("reserved another actor: %+v", attempt.Actor)
	}
}
