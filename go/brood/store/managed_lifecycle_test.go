package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

// FEATURE: Durable, fenced cleanup and destructive reset stages.
// SPEC: Unknown tasks cannot be replaced; stop and delete have separate UPIDs;
// reset keeps old ownership until confirmed absence and never releases capacity.
// IMPLEMENTATION: Persist task stages and require explicit terminal reconciliation.
// RESULT: IMPLEMENTED — cleanup and reset retain ownership and capacity until confirmed.
// STATUS: DONE
func TestManagedLifecycleUnknownTaskBlocksCleanup(t *testing.T) {
	for _, pending := range []string{"create", "start"} {
		for _, operation := range []string{"delete", "reset", "stop", "start"} {
			t.Run(pending+"/"+operation, func(t *testing.T) {
				ctx := context.Background()
				s := newTestStore(t)
				managedFixture(t, s)
				a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
				if err != nil {
					t.Fatal(err)
				}
				if pending == "start" {
					a = lifecycleConfirm(t, s, a, "stopped")
					a = lifecycleBegin(t, s, a, "start")
				}
				a.UPID = "UPID:unknown"
				a.Phase = "running"
				a, err = s.UpdateManagedActor(ctx, a, a.Revision)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "replacement", operation); err == nil {
					t.Fatal("unknown task discarded")
				}
				got, err := s.GetManagedActor(ctx, a.UID)
				if err != nil || !reflect.DeepEqual(got, a) {
					t.Fatalf("evidence changed: %+v %v", got, err)
				}
			})
		}
	}
}

func lifecycleConfirm(t *testing.T, s *Store, a ManagedActor, observation string) ManagedActor {
	t.Helper()
	b, err := s.ConfirmManagedActorStage(context.Background(), a.UID, a.Revision, a.OperationID, a.Stage, observation)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func lifecycleBind(t *testing.T, s *Store, a ManagedActor, upid string) ManagedActor {
	t.Helper()
	b, err := s.BindManagedActorTask(context.Background(), a.UID, a.Revision, a.OperationID, a.Stage, upid)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func lifecycleBegin(t *testing.T, s *Store, a ManagedActor, operation string) ManagedActor {
	t.Helper()
	b, err := s.BeginManagedActorOperation(context.Background(), a.UID, a.Revision, "intent-"+operation, operation)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestManagedLifecycleResetStagesAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	p := managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	a = lifecycleConfirm(t, s, a, "running")
	old := a
	a = lifecycleBegin(t, s, a, "reset")
	if a.Stage != "stop" || a.Incarnation != old.Incarnation || a.OwnerNonce != old.OwnerNonce || a.CTID != old.CTID {
		t.Fatalf("early ownership change: %+v", a)
	}
	a = lifecycleBind(t, s, a, "UPID:stop")
	stop := a
	if _, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh-nonce"); err == nil {
		t.Fatal("reset before absence")
	}
	if _, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "UPID:delete"); err == nil {
		t.Fatal("bound task replaced")
	}
	if _, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "running"); err == nil {
		t.Fatal("running stop advanced")
	}
	if _, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, "wrong", a.Stage, "stopped"); err == nil {
		t.Fatal("wrong operation advanced")
	}
	a = lifecycleConfirm(t, s, a, "stopped")
	if a.Stage != "delete" || a.UPID != "" || a.Incarnation != old.Incarnation || a.OwnerNonce != old.OwnerNonce {
		t.Fatalf("delete stage: %+v", a)
	}
	if _, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "stop", "stopped"); err == nil {
		t.Fatal("stale stage advanced with current revision")
	}
	if _, err = s.ConfirmManagedActorStage(ctx, a.UID, stop.Revision, stop.OperationID, stop.Stage, "stopped"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale callback: %v", err)
	}
	a = lifecycleBind(t, s, a, "UPID:delete")
	a.Phase = "error"
	a.Error = "uncertain delete"
	a, err = s.UpdateManagedActor(ctx, a, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetManagedActor(ctx, a.UID)
	if err != nil || !reflect.DeepEqual(got, a) {
		t.Fatalf("restart: %+v %v", got, err)
	}
	p.MaxActors = 1
	p.Resources = a.Resources
	if _, err = s.UpdateManagedPool(ctx, p, p.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveManagedActor(ctx, managedRequest("b", "two", 102)); !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("reset released capacity: %v", err)
	}
	if _, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, old.OwnerNonce); err == nil {
		t.Fatal("reused owner nonce")
	}
	a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh-nonce")
	if err != nil {
		t.Fatal(err)
	}
	if a.Stage != "create" || a.Operation != "reset" || a.Incarnation != old.Incarnation+1 || a.OwnerNonce != "fresh-nonce" || a.UPID != "" || a.UID != old.UID || a.Resources != old.Resources || a.CTID != old.CTID || a.Phase != "provisioning" {
		t.Fatalf("fresh create: %+v", a)
	}
	if _, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "another-nonce"); err == nil {
		t.Fatal("reset advanced twice")
	}
	if _, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "delete", "absent"); err == nil {
		t.Fatal("old deletion advanced new create")
	}
	a = lifecycleBind(t, s, a, "UPID:fresh-create")
	a = lifecycleConfirm(t, s, a, "stopped")
	if a.Stage != "done" || a.Phase != "stopped" {
		t.Fatalf("fresh CT: %+v", a)
	}
	a = lifecycleBegin(t, s, a, "start")
	a = lifecycleBind(t, s, a, "UPID:start")
	a = lifecycleConfirm(t, s, a, "running")
	if a.OwnerNonce != "fresh-nonce" || a.Incarnation != 2 {
		t.Fatal("start changed workspace ownership")
	}
}
func TestManagedLifecycleDeleteStages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	a = lifecycleConfirm(t, s, a, "running")
	a = lifecycleBegin(t, s, a, "delete")
	if _, err = s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID); err == nil {
		t.Fatal("stop task bypassed")
	}
	a = lifecycleBind(t, s, a, "UPID:stop")
	a = lifecycleConfirm(t, s, a, "stopped")
	a = lifecycleBind(t, s, a, "UPID:delete")
	if a.Stage != "delete" {
		t.Fatalf("stage: %+v", a)
	}
	a, err = s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Phase != "deleted" || a.Stage != "done" {
		t.Fatalf("tombstone: %+v", a)
	}
}

// SPEC: Every task mutation uses revision, operation ID, and stage fences.
func TestManagedLifecycleTaskFencesAndValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []struct {
		revision               int64
		operation, stage, upid string
	}{
		{a.Revision - 1, a.OperationID, a.Stage, "UPID:stale"},
		{a.Revision, "", a.Stage, "UPID:empty-operation"},
		{a.Revision, "wrong", a.Stage, "UPID:wrong-operation"},
		{a.Revision, a.OperationID, "start", "UPID:wrong-stage"},
		{a.Revision, a.OperationID, a.Stage, ""},
	} {
		if _, err = s.BindManagedActorTask(ctx, a.UID, args.revision, args.operation, args.stage, args.upid); err == nil {
			t.Fatalf("invalid bind accepted: %+v", args)
		}
	}
	for _, observed := range []string{"", "unknown", "deleting"} {
		if _, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, observed); err == nil {
			t.Fatalf("invalid observation %q accepted", observed)
		}
	}
	got, err := s.GetManagedActor(ctx, a.UID)
	if err != nil || !reflect.DeepEqual(a, got) {
		t.Fatalf("invalid calls mutated evidence: %+v %v", got, err)
	}
	a = lifecycleBind(t, s, a, "UPID:create")
	if _, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision-1, a.OperationID, a.Stage, "stopped"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale confirm: %v", err)
	}
	a = lifecycleConfirm(t, s, a, "stopped")
	if _, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "UPID:late"); err == nil {
		t.Fatal("completed task rebound")
	}
}

// SPEC: A diagnostic update cannot attach new work to an already reconciled stage.
func TestManagedLifecycleSettledObservationCannotBindTask(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	a = lifecycleConfirm(t, s, a, "stopped")
	a.UPID = "UPID:late-create"
	if _, err = s.UpdateManagedActor(ctx, a, a.Revision); err == nil {
		t.Fatal("late task attached to reconciled stage")
	}
}

// BUG: Завершённые с ошибкой stop/delete блокируют CT и резерв навсегда.
// HYPOTHESIS: Подтверждение успеха не умеет безопасно закрыть слот неудачной задачи.
// SOLUTION: Явное подтверждение терминальной ошибки проверяет все ограды задачи,
// закрывает слот и сохраняет владение, намерение, резерв и доказательства ошибки.
// RESULT: FIXED — все пять сценариев допускают новую попытку с новым ID операции.
// STATUS: FIXED
func TestManagedLifecycleTerminalFailureRetry(t *testing.T) {
	for _, operation := range []string{"stop", "delete", "reset"} {
		stages := []string{"stop"}
		if operation != "stop" {
			stages = append(stages, "delete")
		}
		for _, stage := range stages {
			t.Run(operation+"/"+stage, func(t *testing.T) {
				ctx := context.Background()
				s := newTestStore(t)
				p := managedFixture(t, s)
				a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
				if err != nil {
					t.Fatal(err)
				}
				a = lifecycleConfirm(t, s, a, "running")
				a = lifecycleBegin(t, s, a, operation)
				if stage == "delete" {
					a = lifecycleBind(t, s, a, "UPID:stop")
					a = lifecycleConfirm(t, s, a, "stopped")
				}
				a = lifecycleBind(t, s, a, "UPID:failed-"+stage)
				a.Phase = "running"
				a, err = s.UpdateManagedActor(ctx, a, a.Revision)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "retry", operation); err == nil {
					t.Fatal("diagnostic running phase replaced unresolved task")
				}
				a.Phase, a.Error = "error", "terminal task FAILED"
				a, err = s.UpdateManagedActor(ctx, a, a.Revision)
				if err != nil {
					t.Fatal(err)
				}
				old := a
				observed := "running"
				if stage == "delete" {
					observed = "stopped"
				}
				if _, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, stage, observed); err == nil {
					t.Fatal("success confirmation accepted failed target")
				}
				if _, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "retry", operation); err == nil {
					t.Fatal("unresolved task replaced")
				}
				resolver, ok := any(s).(managedTaskFailureResolver)
				if !ok {
					t.Fatal("terminal failed task has no explicit fenced failure-resolution API")
				}
				a, err = resolver.ResolveManagedActorTaskFailure(ctx, a.UID, a.Revision, a.OperationID, stage, a.UPID, observed)
				if err != nil {
					t.Fatal(err)
				}
				want := old
				want.Stage, want.Phase = "done", "error"
				want.Revision, want.UpdatedAt = a.Revision, a.UpdatedAt
				if a.Revision != old.Revision+1 || !reflect.DeepEqual(a, want) {
					t.Fatalf("failure changed evidence or ownership: %+v", a)
				}
				if operation == "delete" {
					for _, cancel := range []string{"start", "stop", "reset"} {
						if _, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "cancel", cancel); err == nil {
							t.Fatal("delete intent canceled")
						}
					}
				}
				p.MaxActors, p.Resources = 1, a.Resources
				if _, err = s.UpdateManagedPool(ctx, p, p.Revision); err != nil {
					t.Fatal(err)
				}
				assertReserved := func() {
					t.Helper()
					if _, err := s.ReserveManagedActor(ctx, managedRequest("b", "two", 102)); !errors.Is(err, ErrPoolCapacity) {
						t.Fatalf("failure or retry released capacity: %v", err)
					}
				}
				assertReserved()
				a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "retry", operation)
				if err != nil {
					t.Fatalf("fresh retry refused: %v", err)
				}
				if a.Stage != "stop" || a.UPID != "" || a.OperationID == old.OperationID || a.Incarnation != old.Incarnation || a.OwnerNonce != old.OwnerNonce || a.CTID != old.CTID || a.Resources != old.Resources {
					t.Fatalf("unsafe retry: %+v", a)
				}
				assertReserved()
				a = lifecycleBind(t, s, a, "UPID:retry-stop")
				if _, err = resolver.ResolveManagedActorTaskFailure(ctx, a.UID, old.Revision, old.OperationID, old.Stage, old.UPID, observed); !errors.Is(err, ErrRevisionConflict) {
					t.Fatalf("stale revision: %v", err)
				}
				if _, err = resolver.ResolveManagedActorTaskFailure(ctx, a.UID, a.Revision, old.OperationID, old.Stage, old.UPID, observed); err == nil {
					t.Fatal("old callback settled retry")
				}
				if _, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, old.OperationID, "stop", "stopped"); err == nil {
					t.Fatal("old success callback settled retry")
				}
				a = lifecycleConfirm(t, s, a, "stopped")
				if operation == "reset" {
					a = lifecycleBind(t, s, a, "UPID:retry-delete")
					a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh-retry-nonce")
					if err != nil || a.Incarnation != old.Incarnation+1 {
						t.Fatalf("reset retry: %+v %v", a, err)
					}
					assertReserved()
				} else if operation == "delete" {
					a = lifecycleBind(t, s, a, "UPID:retry-delete")
					if _, err = s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

type managedTaskFailureResolver interface {
	ResolveManagedActorTaskFailure(context.Context, string, int64, string, string, string, string) (ManagedActor, error)
}

func TestManagedLifecycleFailureResolutionValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	resolver, ok := any(s).(managedTaskFailureResolver)
	if !ok {
		t.Fatal("terminal failed task has no explicit fenced failure-resolution API")
	}
	if _, err = resolver.ResolveManagedActorTaskFailure(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "UPID:unbound", "absent"); err == nil {
		t.Fatal("unbound failure accepted")
	}
	a = lifecycleBind(t, s, a, "UPID:create")
	for _, args := range []struct {
		revision                         int64
		operation, stage, upid, observed string
	}{
		{a.Revision - 1, a.OperationID, a.Stage, a.UPID, "stopped"},
		{a.Revision, "", a.Stage, a.UPID, "stopped"},
		{a.Revision, "stale", a.Stage, a.UPID, "stopped"},
		{a.Revision, a.OperationID, "stop", a.UPID, "stopped"},
		{a.Revision, a.OperationID, a.Stage, "UPID:stale", "stopped"},
		{a.Revision, a.OperationID, a.Stage, "", "stopped"},
		{a.Revision, a.OperationID, a.Stage, a.UPID, "unknown"},
		{a.Revision, a.OperationID, a.Stage, a.UPID, ""},
		{a.Revision, a.OperationID, a.Stage, a.UPID, "deleted"},
	} {
		if _, err = resolver.ResolveManagedActorTaskFailure(ctx, a.UID, args.revision, args.operation, args.stage, args.upid, args.observed); err == nil {
			t.Fatalf("invalid failure accepted: %+v", args)
		}
	}
	got, err := s.GetManagedActor(ctx, a.UID)
	if err != nil || !reflect.DeepEqual(got, a) {
		t.Fatalf("invalid failure changed evidence: %+v %v", got, err)
	}
	for _, observed := range []string{"running", "stopped", "absent"} {
		for _, operation := range []string{"start", "stop", "reset"} {
			t.Run(observed+"/"+operation, func(t *testing.T) {
				// Откат сохраняет один исходный слот для всех проверок.
				rollback := errors.New("rollback")
				err := s.WithTx(ctx, func(ctx context.Context) error {
					b, err := resolver.ResolveManagedActorTaskFailure(ctx, a.UID, a.Revision, a.OperationID, a.Stage, a.UPID, observed)
					if err != nil {
						t.Fatal(err)
					}
					if b.Stage != "done" || b.Phase != "error" || b.UPID != a.UPID {
						t.Fatalf("failure not retained: %+v", b)
					}
					if _, err = resolver.ResolveManagedActorTaskFailure(ctx, b.UID, b.Revision, b.OperationID, b.Stage, b.UPID, observed); err == nil {
						t.Fatal("done stage resolved again")
					}
					if _, err := s.BeginManagedActorOperation(ctx, b.UID, b.Revision, "fresh-"+operation, operation); err != nil {
						t.Fatalf("reconciled error cannot begin %s: %v", operation, err)
					}
					return rollback
				})
				if !errors.Is(err, rollback) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestManagedLifecycleFailedCreateReconciliation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	a = lifecycleBind(t, s, a, "UPID:failed-create")
	// The caller confirms the task is terminal and no CT exists before cleanup.
	a = lifecycleConfirm(t, s, a, "absent")
	a = lifecycleBegin(t, s, a, "delete")
	a = lifecycleConfirm(t, s, a, "absent")
	if a.Stage != "delete" {
		t.Fatalf("absence did not fence stop stage: %+v", a)
	}
	if _, err = s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID); err != nil {
		t.Fatal(err)
	}
}
