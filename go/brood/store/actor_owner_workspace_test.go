package store

// BUG: A late automatic workspace acknowledgement can release an operation
// that the owner interrupted, admitting new automatic effects without reconciliation.
// HYPOTHESIS: Owner evidence marks parent steps but not existing workspace claims.
// SOLUTION: Mark held workspace operations unconfirmed and fence late acknowledgements.
// RESULT: FIXED — owner control remains available; automatic holds cannot be forgotten.
// STATUS: FIXED

import (
	"context"
	"errors"
	"testing"
)

func TestOwnerResetRetainsOldChildIncarnation(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	c := reserveChild(t, s, p, e, "old-incarnation")
	c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
	mustNil(t, err)
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	a, err = s.BeginOwnerManagedActorOperation(ctx, a, "owner-reset", "reset", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	a, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "stop", "stopped")
	mustNil(t, err)
	a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh-reset-owner")
	mustNil(t, err)
	a, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "create", "running")
	mustNil(t, err)
	a, err = s.BeginOwnerManagedActorOperation(ctx, a, "owner-start", "start", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	a, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "start", "running")
	mustNil(t, err)
	if a.Incarnation != e.Incarnation+1 {
		t.Fatalf("reset did not rotate incarnation: %+v", a)
	}
	next := e
	next.Incarnation = a.Incarnation
	for _, caller := range []SwarmChildPrincipal{e, next} {
		if _, err := s.CompleteSwarmChild(ctx, p, c.RequestID, caller, c.Revision, "success", "old child report", childAllow); !errors.Is(err, ErrSwarmChildDenied) {
			t.Fatalf("reset incarnation completed historical child: %+v %v", caller, err)
		}
	}
	w, err := s.SwarmChildWorkByID(ctx, c.ID)
	mustNil(t, err)
	if w.Task.State != "running" || w.Task.Executor != e {
		t.Fatalf("reset rewrote or orphaned historical child: %+v", w)
	}
}

func TestOwnerLateChildReceiptDoesNotEraseInterruption(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: p.Principal.SwarmID, Role: "review", ActorUID: e.ActorUID}))
	c, err := s.EnqueueSwarmChild(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}, "receipt", "review", "task", childAllow)
	mustNil(t, err)
	c, err = s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
	mustNil(t, err)
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	op, err := s.AcquireOwnerActorOperation(ctx, a, "owner-input", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	mustNil(t, s.RecordSwarmChildDelivery(ctx, p, c.RequestID, "delivered", "late receipt"))
	j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if !j.Interrupted || j.Evaluation.State != "unconfirmed" || j.Children[0].Delivery != "delivered" || j.Children[0].State != "running" {
		t.Fatalf("late receipt erased interruption or task evidence: %+v", j)
	}
	c = j.Children[0]
	_, err = s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "late report", childAllow)
	mustNil(t, err)
	j, err = s.JoinSwarmChildren(ctx, p, childAllow)
	mustNil(t, err)
	if !j.Interrupted || j.Evaluation.State != "unconfirmed" {
		t.Fatalf("late report erased intervention: %+v", j)
	}
}

func TestOwnerInterventionFencesLateWorkspaceSettlement(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	automatic, err := s.AcquireActorOperation(ctx, a.UID, a.Incarnation, "automatic", "guest", "restart")
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	owner, err := s.AcquireOwnerActorOperation(ctx, a, "owner", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, owner, true))
	if err := s.FinishActorOperation(ctx, automatic, true); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("late acknowledgement released interrupted automatic effect: %v", err)
	}
	retained, err := s.CurrentActorOperation(ctx, a.UID)
	mustNil(t, err)
	if retained.OperationID != automatic.OperationID || retained.State != "unconfirmed" {
		t.Fatalf("interrupted workspace evidence lost: %+v", retained)
	}
	if _, err := s.ReserveSwarmChild(ctx, p, "new-child", "task", e, childAllow); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("late acknowledgement admitted automatic child: %v", err)
	}
	// The retained automatic hold must not deny the next explicit owner action.
	next, err := s.AcquireOwnerActorOperation(ctx, a, "owner-next", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, next, true))
}
