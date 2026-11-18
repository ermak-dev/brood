package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// FEATURE: Явный owner reset может закончиться первым start, не снимая child holds.
// SPEC: Только точная сохранённая derived reset lineage и reconciled snapshot;
// nil/отозванный callback, иной scope, revision и failed/missing root отказывают.
// IMPLEMENTATION: Точный derived reset и повторная проверка после callback, atomic start/link.
// RESULT: IMPLEMENTED — child/workspace holds остаются, повторный start не получает root.
// STATUS: DONE
func TestOwnerResetContinuationRetainsHoldsAndOriginalRoot(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	reserveChild(t, s, p, e, "running")
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, a.SwarmID)
	mustNil(t, err)
	a, err = s.BeginOwnerManagedActorOperation(ctx, a, "owner-reset-continuation", "reset", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "absent")
	a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "new-owner-nonce")
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	if _, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "ordinary-start", "start"); err == nil {
		t.Fatal("ordinary automatic start bypassed child hold")
	}
	before := databaseRows(t, s.db)
	calls := 0
	next, err := s.ContinueOwnerManagedActorReset(ctx, a, "continued-owner-start", func(tx context.Context, link OwnerManagedActorLineage) error {
		calls++
		current, err := s.GetManagedActor(tx, a.UID)
		if err != nil {
			return err
		}
		if current != a || link.Origin.OperationID != "owner-reset-continuation" {
			t.Fatalf("callback did not see exact predecessor: %+v %+v", current, link)
		}
		return nil
	})
	mustNil(t, err)
	if calls != 1 || next.Operation != "start" || next.Stage != "start" || next.Incarnation != a.Incarnation || next.OperationID != "continued-owner-start" {
		t.Fatalf("continuation failed: %+v calls=%d", next, calls)
	}
	link, err := s.GetDerivedOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, next.OperationID)
	mustNil(t, err)
	if link.Operation != "start" || link.Origin.OperationID != "owner-reset-continuation" || link.PriorOperationID != a.OperationID {
		t.Fatalf("continuation lost root: %+v", link)
	}
	after := databaseRows(t, s.db)
	for _, table := range []string{"actor_interventions", "actor_step_interruptions", "coordination_claims", "swarm_child_tasks", "swarm_child_joins", "swarm_steps"} {
		if !reflect.DeepEqual(before[table], after[table]) {
			t.Fatalf("owner continuation cleared or rewrote %s", table)
		}
	}
	if _, err := s.GetOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, next.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("continuation fabricated direct intervention: %v", err)
	}
	if _, err := s.ContinueOwnerManagedActorReset(ctx, a, "continued-owner-start", func(context.Context, OwnerManagedActorLineage) error { return nil }); err == nil {
		t.Fatal("stale predecessor reused as fresh continuation")
	}
}

func TestOwnerResetContinuationRetainsInterruptedWorkspace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := ownerHistoryActor(t, s)
	work, err := s.AcquireActorOperation(ctx, a.UID, a.Incarnation, "old-workspace", "automatic", "restart")
	mustNil(t, err)
	a = ownerResetForLineage(t, s, a, "workspace-owner-reset")
	a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "workspace-new-nonce")
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	held, err := s.CurrentActorOperation(ctx, a.UID)
	mustNil(t, err)
	if held.OperationID != work.OperationID || held.State != "unconfirmed" || held.InterruptedBy != "workspace-owner-reset" || held.Generation != 1 {
		t.Fatalf("workspace hold was not established: %+v", held)
	}
	next, err := s.ContinueOwnerManagedActorReset(ctx, a, "workspace-first-start", func(context.Context, OwnerManagedActorLineage) error { return nil })
	mustNil(t, err)
	after, err := s.CurrentActorOperation(ctx, a.UID)
	mustNil(t, err)
	if after != held {
		t.Fatalf("continuation changed unknown workspace: %+v -> %+v", held, after)
	}
	next = lifecycleConfirm(t, s, next, "running")
	if _, err := s.AcquireActorOperation(ctx, next.UID, next.Incarnation, "new-automatic-work", "automatic", "restart"); err == nil {
		t.Fatal("owner continuation released the automatic workspace hold")
	}
}

func TestOwnerResetContinuationDoesNotReuseReturnedStart(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"pending", "completed", "failed"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			a := ownerHistoryActor(t, s)
			a = ownerResetForLineage(t, s, a, "original-owner-reset")
			a, err := s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh-nonce")
			mustNil(t, err)
			a = lifecycleConfirm(t, s, a, "stopped")
			a, err = s.ContinueOwnerManagedActorReset(ctx, a, "one-owner-first-start", func(context.Context, OwnerManagedActorLineage) error { return nil })
			mustNil(t, err)
			if state != "pending" {
				a, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "one-start-task")
				mustNil(t, err)
				if state == "completed" {
					a, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "running")
				} else {
					a, err = s.ResolveManagedActorTaskFailure(ctx, a.UID, a.Revision, a.OperationID, a.Stage, a.UPID, "stopped")
				}
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			if _, err := s.ContinueOwnerManagedActorReset(ctx, a, "another-start", func(context.Context, OwnerManagedActorLineage) error { return nil }); err == nil {
				t.Fatal("returned start reused the original reset authorization")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("reused start changed durable state")
			}
		})
	}
}

func TestOwnerResetContinuationRefusesInvalidEvidenceAtomically(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"nil-authority", "denied", "stale", "identity", "missing", "failed", "empty-id", "same-id", "ignored-link", "ignored-update", "callback-revision", "callback-namespace", "outer-rollback"} {
		t.Run(scenario, func(t *testing.T) {
			s := newTestStore(t)
			a := ownerHistoryActor(t, s)
			if scenario == "missing" {
				a = lifecycleBegin(t, s, a, "reset")
				a = lifecycleConfirm(t, s, a, "absent")
			} else {
				a = ownerResetForLineage(t, s, a, "explicit-owner-reset")
			}
			var err error
			a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "new-nonce")
			mustNil(t, err)
			if scenario == "failed" {
				a, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "failed-create")
				mustNil(t, err)
				a, err = s.ResolveManagedActorTaskFailure(ctx, a.UID, a.Revision, a.OperationID, a.Stage, a.UPID, "stopped")
				mustNil(t, err)
			} else {
				a = lifecycleConfirm(t, s, a, "stopped")
			}
			target := a
			id := "owner-first-start"
			callback := func(context.Context, OwnerManagedActorLineage) error { return nil }
			switch scenario {
			case "nil-authority":
				callback = nil
			case "denied":
				callback = func(context.Context, OwnerManagedActorLineage) error { return ErrSwarmChildDenied }
			case "stale":
				target.Revision++
			case "identity":
				target.AtespaceUID = "other-space"
			case "empty-id":
				id = ""
			case "same-id":
				id = a.OperationID
			case "callback-revision":
				callback = func(tx context.Context, _ OwnerManagedActorLineage) error {
					changed := target
					changed.Error = "changed during authorization"
					_, err := s.UpdateManagedActor(tx, changed, target.Revision)
					return err
				}
			case "callback-namespace":
				callback = func(tx context.Context, _ OwnerManagedActorLineage) error {
					other := "replacement-space"
					return s.SetSwarmAtespaceUID(tx, target.SwarmID, &other)
				}
			case "ignored-link":
				_, err = s.db.Exec(`CREATE TRIGGER ignore_continuation BEFORE INSERT ON owner_managed_actor_lineage BEGIN SELECT RAISE(IGNORE); END`)
				mustNil(t, err)
			case "ignored-update":
				_, err = s.db.Exec(`CREATE TRIGGER ignore_continuation BEFORE UPDATE ON managed_actors BEGIN SELECT RAISE(IGNORE); END`)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			txErr := s.WithTx(ctx, func(tx context.Context) error {
				_, err := s.ContinueOwnerManagedActorReset(tx, target, id, callback)
				if scenario == "outer-rollback" {
					mustNil(t, err)
					return errors.New("outer rollback")
				}
				if err == nil {
					t.Fatal("invalid owner continuation accepted")
				}
				return nil
			})
			if scenario == "outer-rollback" {
				if txErr == nil {
					t.Fatal("outer rollback accepted")
				}
			} else {
				mustNil(t, txErr)
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refused continuation leaked state")
			}
		})
	}
}
