package store

// BUG: Child scheduling holds deny explicit owner session control.
// HYPOTHESIS: Automatic allocation exclusion is incorrectly used as owner authority.
// SOLUTION: Journal authorized owner effects separately and retain interrupted attempts.

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

// FEATURE: Точное историческое свидетельство явного managed-вмешательства owner/admin.
// SPEC: Только start/stop/delete/reset/update и точные UID, incarnation, operation ID;
// завершённая история переживает reopen и отзыв прав, чтение не даёт разрешений и не пишет.
// IMPLEMENTATION: Отдельный SELECT журнала по точному ключу и списку lifecycle-операций.
// RESULT: IMPLEMENTED — точная история сохраняется, чтение не меняет журнал и инвентарь.
// STATUS: DONE
func TestGetOwnerManagedActorOperationLifecycleHistory(t *testing.T) {
	ctx := context.Background()
	for _, human := range []string{"u", "admin"} {
		for _, kind := range []string{"start", "stop", "delete", "reset", "update"} {
			t.Run(human+"/"+kind, func(t *testing.T) {
				s := newTestStore(t)
				a := ownerHistoryActor(t, s)
				if human == "admin" {
					seedUser(t, s, human, human)
					_, err := s.db.Exec(`UPDATE users SET is_admin=1 WHERE id=?`, human)
					mustNil(t, err)
				}
				next, err := s.BeginOwnerManagedActorOperation(ctx, a, "server-operation", kind, human, func(context.Context) error { return nil })
				mustNil(t, err)
				want := ActorOperation{ProjectID: "one", SwarmID: a.SwarmID, UID: a.UID, Generation: a.Incarnation, OperationID: "server-operation", Caller: "operator:" + human, Operation: kind, State: "completed", Owner: true}
				before := databaseRows(t, s.db)
				got, err := s.GetOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, next.OperationID)
				if err != nil || got != want {
					t.Fatalf("exact lifecycle history: got %+v, want %+v, err %v", got, want, err)
				}
				if kind == "update" && next.Operation != "start" {
					t.Fatalf("managed update mapping changed: %+v", next)
				}
				if _, err := s.CurrentActorOperation(ctx, a.UID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("completed intent became active workspace permission: %v", err)
				}
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("historical lookup changed durable state")
				}
			})
		}
	}
}

// ownerHistoryActor готовит обычного persistent-актора штатными managed API.
func ownerHistoryActor(t *testing.T, s *Store) ManagedActor {
	t.Helper()
	ctx := context.Background()
	managedFixture(t, s)
	mustNil(t, s.AddMember(ctx, "one", "u"))
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	a = lifecycleBegin(t, s, a, "start")
	return lifecycleConfirm(t, s, a, "running")
}

func TestGetOwnerManagedActorOperationExactReadOnlyHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "owner-history.db")
	s, err := Open(ctx, path)
	mustNil(t, err)
	t.Cleanup(func() {
		if s != nil {
			s.Close()
		}
	})
	a := ownerHistoryActor(t, s)
	a, err = s.BeginOwnerManagedActorOperation(ctx, a, "owner-stop", "stop", "u", func(context.Context) error { return nil })
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	// Более поздний явный intent не должен подменить точную старую запись.
	a, err = s.BeginOwnerManagedActorOperation(ctx, a, "owner-reset", "reset", "u", func(context.Context) error { return nil })
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "absent")
	a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh-owner")
	mustNil(t, err)
	if a.Incarnation != 2 {
		t.Fatalf("reset did not rotate incarnation: %+v", a)
	}
	// Историческое свидетельство не зависит от сегодняшних прав человека.
	_, err = s.db.Exec(`UPDATE users SET disabled=1 WHERE id='u'; DELETE FROM project_members WHERE user_id='u'`)
	mustNil(t, err)
	mustNil(t, s.Close())
	s, err = Open(ctx, path)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	if err := s.actorOwner(ctx, a.UID, "u"); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatalf("test human still has current authority: %v", err)
	}
	_, err = s.db.Exec(`PRAGMA query_only=ON`)
	mustNil(t, err)
	for _, id := range []string{"owner-stop", "owner-reset"} {
		got, err := s.GetOwnerManagedActorOperation(ctx, a.UID, 1, id)
		want := ActorOperation{ProjectID: "one", SwarmID: "one", UID: a.UID, Generation: 1, OperationID: id, Caller: "operator:u", Operation: id[len("owner-"):], State: "completed", Owner: true}
		if err != nil || got != want {
			t.Fatalf("retained history after reopen: got %+v, want %+v, err %v", got, want, err)
		}
	}
	for _, tc := range []struct {
		uid string
		inc int64
		id  string
	}{
		{"", 1, "owner-stop"}, {a.UID, 0, "owner-stop"}, {a.UID, -1, "owner-stop"}, {a.UID, 1, ""},
		{"wrong", 1, "owner-stop"}, {a.UID, 3, "owner-stop"}, {a.UID, 1, "missing"},
		{a.Name + "-name", 1, "owner-stop"}, {a.UID, 1, "owner-stop "},
		// Reset не создаёт свидетельство owner для новой инкарнации.
		{a.UID, a.Incarnation, "owner-reset"},
	} {
		got, err := s.GetOwnerManagedActorOperation(ctx, tc.uid, tc.inc, tc.id)
		if !errors.Is(err, ErrNotFound) || got != (ActorOperation{}) {
			t.Fatalf("nonexact identity %+v returned %+v, %v", tc, got, err)
		}
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("read-only historical lookup changed durable state")
	}
}

func TestGetOwnerManagedActorOperationExcludesWorkspaceAndUnrecorded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := ownerHistoryActor(t, s)
	for _, kind := range []string{"slash", "session_terminal", "terminal_stream", "restart", "wipe", "recycle"} {
		op, err := s.AcquireOwnerActorOperation(ctx, a, "owner-"+kind, "operator:u", kind, "u", func(context.Context) error { return nil })
		mustNil(t, err)
		mustNil(t, s.FinishActorOperation(ctx, op, true))
	}
	a, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "automatic-stop", "stop")
	mustNil(t, err)
	before := databaseRows(t, s.db)
	for _, id := range []string{"owner-slash", "owner-session_terminal", "owner-terminal_stream", "owner-restart", "owner-wipe", "owner-recycle", a.OperationID, "create-a", "intent-start"} {
		got, err := s.GetOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, id)
		if !errors.Is(err, ErrNotFound) || got != (ActorOperation{}) {
			t.Fatalf("non-lifecycle/unrecorded %s classified owner: %+v, %v", id, got, err)
		}
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("missing evidence lookup changed durable state")
	}
}

func TestGetOwnerManagedActorOperationDoesNotInferOwner(t *testing.T) {
	ctx := context.Background()
	s, persistent, ephemeral := classFixture(t)
	for _, a := range []ManagedActor{persistent, ephemeral} {
		a = lifecycleConfirm(t, s, a, "stopped")
		a = lifecycleBegin(t, s, a, "start")
		a = lifecycleConfirm(t, s, a, "running")
		// Класс, имя и отсутствие automatic provenance не заменяют журнал owner.
		before := databaseRows(t, s.db)
		got, err := s.GetOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID)
		if !errors.Is(err, ErrNotFound) || got != (ActorOperation{}) {
			t.Fatalf("unrecorded %s intent classified owner: %+v, %v", a.Class, got, err)
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("unrecorded lookup wrote provenance")
		}
		op, err := s.AcquireActorOperation(ctx, a.UID, a.Incarnation, "workspace-"+a.UID, "automatic", "restart")
		mustNil(t, err)
		current, err := s.CurrentActorOperation(ctx, a.UID)
		mustNil(t, err)
		if current != op || current.Owner {
			t.Fatalf("workspace setup: %+v", current)
		}
		before = databaseRows(t, s.db)
		got, err = s.GetOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, op.OperationID)
		if !errors.Is(err, ErrNotFound) || got != (ActorOperation{}) {
			t.Fatalf("automatic workspace classified owner: %+v, %v", got, err)
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("workspace lookup changed reservation")
		}
		mustNil(t, s.FinishActorOperation(ctx, op, true))
	}
}

func TestGetOwnerManagedActorOperationRetainsJournalStates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := ownerHistoryActor(t, s)
	op, err := s.AcquireOwnerActorOperation(ctx, a, "owner-pending", "operator:u", "stop", "u", func(context.Context) error { return nil })
	mustNil(t, err)
	for _, state := range []string{"inflight", "unconfirmed"} {
		if state == "unconfirmed" {
			mustNil(t, s.FinishActorOperation(ctx, op, false))
			op.State = state
		}
		before := databaseRows(t, s.db)
		got, err := s.GetOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, op.OperationID)
		if err != nil || got != op {
			t.Fatalf("journal state %s lost: %+v, %v", state, got, err)
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("journal state lookup changed durable state")
		}
	}
}

func TestOwnerLifecycleInterventionAtomicFailure(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	reserveChild(t, s, p, e, "queued")
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	before := dumpTables(t, s, "actor_interventions", "actor_step_interruptions", "swarm_steps", "managed_actors")
	mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
		a.Revision++
		if _, err := s.BeginOwnerManagedActorOperation(ctx, a, "bad-revision", "stop", sw.OwnerID, func(context.Context) error { return nil }); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("wrong revision accepted: %v", err)
		}
		return nil
	}))
	if got := dumpTables(t, s, "actor_interventions", "actor_step_interruptions", "swarm_steps", "managed_actors"); got != before {
		t.Fatal("handled owner refusal leaked intervention inside outer transaction")
	}
}

func TestOwnerInterventionLateCompletionRetainsUncertainty(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	c := reserveChild(t, s, p, e, "running")
	c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
	mustNil(t, err)
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	op, err := s.AcquireOwnerActorOperation(ctx, a, "interrupt", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	// The executor's observed report is preserved, but cannot automatically bless
	// the interrupted parent execution or release the child's automatic hold.
	c, err = s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "late evidence", childAllow)
	mustNil(t, err)
	j, err := s.JoinSwarmChildren(ctx, p, childAllow)
	mustNil(t, err)
	if !j.Interrupted || j.Evaluation.State != "unconfirmed" || c.Report != "late evidence" {
		t.Fatalf("late completion erased uncertainty: %+v", j)
	}
	step, err := s.GetSwarmStep(ctx, p.Principal.SwarmID, p.Sequence)
	mustNil(t, err)
	if step.State != "unconfirmed" {
		t.Fatalf("late success advanced parent: %+v", step)
	}
	if err := s.CheckActorChildObligations(ctx, a.UID); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("interrupted allocation automatically released: %v", err)
	}
	if _, err := s.AcquireOwnerActorOperation(ctx, a, "interrupt", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil }); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("owner effect replay granted a second send: %v", err)
	}
}

func TestOwnerInterventionDoesNotGrantForeignCaller(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	reserveChild(t, s, p, e, "queued")
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	before := dumpTables(t, s, "actor_interventions", "actor_step_interruptions", "swarm_steps")
	for _, human := range []string{"missing", "ed", "vw"} {
		if _, err := s.AcquireOwnerActorOperation(ctx, a, "foreign-"+human, "foreign", "session_terminal", human, func(context.Context) error { return nil }); !errors.Is(err, ErrSwarmChildDenied) {
			t.Fatalf("foreign intervention accepted for %s: %v", human, err)
		}
	}
	if got := dumpTables(t, s, "actor_interventions", "actor_step_interruptions", "swarm_steps"); got != before {
		t.Fatal("foreign refusal changed execution")
	}
}

func TestOwnerCanStopAndResetRunningChild(t *testing.T) {
	for _, operation := range []string{"stop", "reset"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			s, p, e := childFixture(t, "all", 1, 0)
			c := reserveChild(t, s, p, e, "running")
			c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
			mustNil(t, err)
			a, err := s.GetManagedActor(ctx, e.ActorUID)
			mustNil(t, err)
			sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
			mustNil(t, err)
			next, err := s.BeginOwnerManagedActorOperation(ctx, a, "owner-"+operation, operation, sw.OwnerID, func(context.Context) error { return nil })
			mustNil(t, err)
			if next.Phase != "stopping" {
				t.Fatalf("owner lifecycle not recorded: %+v", next)
			}
			if _, err := s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "stale completion", childAllow); !errors.Is(err, ErrSwarmChildDenied) {
				t.Fatalf("stopping incarnation completion accepted: %v", err)
			}
			w, err := s.SwarmChildWorkByID(ctx, c.ID)
			mustNil(t, err)
			if w.Task.State != "running" {
				t.Fatalf("running task orphaned by owner lifecycle: %+v", w)
			}
		})
	}
}

func TestOwnerSessionInterventionWithRunningChild(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 2, 0)
	c := reserveChild(t, s, p, e, "running")
	c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
	mustNil(t, err)
	target, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	op, err := s.AcquireActorSessionOperation(ctx, target, "owner-input", "operator:"+sw.OwnerID, "session_terminal", CoordinationActor{}, sw.OwnerID, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("explicit owner input denied by automatic child hold: %v", err)
	}
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	// The effect cannot turn the running child into success or erase its record.
	j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if len(j.Children) != 1 || j.Children[0].State != "running" {
		t.Fatalf("running history changed: %+v", j)
	}
	step, err := s.GetSwarmStep(ctx, p.Principal.SwarmID, p.Sequence)
	mustNil(t, err)
	if step.State != "unconfirmed" {
		t.Fatalf("parent still claims uninterrupted execution: %s", step.State)
	}
	if _, err := s.AcquireActorOperation(ctx, target.UID, target.Incarnation, "automatic-restart", "automatic", "restart"); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("automatic conflicting mutation allowed: %v", err)
	}
}
