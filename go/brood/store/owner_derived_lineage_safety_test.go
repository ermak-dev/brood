package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// FEATURE: Явная lineage не заменяет authority и не забывает исходный root.
// SPEC: Lookup точный и readonly, история immutable и переживает удаление inventory;
// неизвестный/чужой source и обычные автоматические операции lineage не создают.
func TestOwnerDerivedLineageExactRetainedRead(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := ownerHistoryActor(t, s)
	a = ownerResetForLineage(t, s, a, "root")
	root, err := s.GetOwnerManagedActorOperation(ctx, a.UID, 1, "root")
	mustNil(t, err)
	a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh")
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "start-derived", "start")
	mustNil(t, err)
	want := OwnerManagedActorLineage{UID: a.UID, Incarnation: 2, OperationID: a.OperationID, Operation: "start", PriorIncarnation: 2, PriorOperationID: "root", Origin: root}
	for _, query := range []string{
		`UPDATE owner_managed_actor_lineage SET origin_id='other'`,
		`UPDATE owner_managed_actor_lineage SET incarnation=99`,
		`DELETE FROM owner_managed_actor_lineage`,
		`INSERT OR REPLACE INTO owner_managed_actor_lineage SELECT * FROM owner_managed_actor_lineage`,
		`UPDATE actor_interventions SET incarnation=99 WHERE id='root'`,
		`DELETE FROM actor_interventions WHERE id='root'`,
		`INSERT OR REPLACE INTO actor_interventions SELECT * FROM actor_interventions WHERE id='root'`,
	} {
		if _, err := s.db.Exec(query); err == nil {
			t.Fatalf("history mutation accepted: %s", query)
		}
	}
	_, err = s.db.Exec(`UPDATE users SET disabled=1 WHERE id='u'; DELETE FROM project_members WHERE user_id='u'; DELETE FROM managed_actors WHERE uid=?`, a.UID)
	mustNil(t, err)
	var path string
	mustNil(t, s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path))
	mustNil(t, s.Close())
	s, err = Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	before := databaseRows(t, s.db)
	_, err = s.db.Exec(`PRAGMA query_only=ON`)
	mustNil(t, err)
	got, err := s.GetDerivedOwnerManagedActorOperation(ctx, a.UID, 2, a.OperationID)
	if err != nil || got != want {
		t.Fatalf("retained derived evidence: %+v, %v; want %+v", got, err, want)
	}
	reset, err := s.GetDerivedOwnerManagedActorOperation(ctx, a.UID, 2, "root")
	mustNil(t, err)
	if reset.Origin != root || reset.Operation != "reset" || reset.PriorIncarnation != 1 || reset.PriorOperationID != "root" {
		t.Fatalf("reset link: %+v", reset)
	}
	for _, key := range []struct {
		uid string
		inc int64
		id  string
	}{
		{"", 2, "root"}, {a.UID, 0, "root"}, {a.UID, -1, "root"}, {a.UID, 2, ""}, {"other", 2, "root"}, {a.UID, 1, "root"}, {a.UID, 3, "root"}, {a.UID, 2, "missing"}, {a.UID, 2, "root "},
	} {
		got, err := s.GetDerivedOwnerManagedActorOperation(ctx, key.uid, key.inc, key.id)
		if !errors.Is(err, ErrNotFound) || got != (OwnerManagedActorLineage{}) {
			t.Fatalf("nonexact %+v: %+v %v", key, got, err)
		}
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("read wrote history")
	}
}

func TestOwnerDerivedLineageRequiresPositiveResetSource(t *testing.T) {
	ctx := context.Background()
	for _, source := range []string{"missing", "inflight", "unconfirmed", "stop", "wrong-incarnation", "wrong-uid", "wrong-swarm", "wrong-project"} {
		t.Run(source, func(t *testing.T) {
			s := newTestStore(t)
			a := ownerHistoryActor(t, s)
			a = lifecycleBegin(t, s, a, "reset")
			a = lifecycleConfirm(t, s, a, "absent")
			if source != "missing" {
				uid, inc, sw, project, kind, state := a.UID, a.Incarnation, a.SwarmID, "one", "reset", "completed"
				switch source {
				case "inflight", "unconfirmed":
					state = source
				case "stop":
					kind = "stop"
				case "wrong-incarnation":
					inc++
				case "wrong-uid":
					uid = "other"
				case "wrong-swarm":
					sw = "other"
				case "wrong-project":
					project = "other"
				}
				_, err := s.db.Exec(`INSERT INTO actor_interventions VALUES(?,?,?,?,?,?,?,?,?,?)`, a.OperationID, project, sw, uid, inc, "u", "operator:u", kind, state, "now")
				mustNil(t, err)
			}
			a, err := s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh")
			mustNil(t, err)
			_, err = s.GetDerivedOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("nonpositive %s created lineage: %v", source, err)
			}
			var n int
			mustNil(t, s.db.QueryRow(`SELECT count(*) FROM owner_managed_actor_lineage`).Scan(&n))
			if n != 0 {
				t.Fatal("invented manual intent")
			}
		})
	}
}

func TestOwnerDerivedLineageAutomaticResetRemainsAutomatic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := ownerHistoryActor(t, s)
	a = lifecycleBegin(t, s, a, "reset")
	a = lifecycleConfirm(t, s, a, "absent")
	a, err := s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh")
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "automatic-first-start", "start")
	mustNil(t, err)
	if _, err := s.GetDerivedOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invented automatic root: %v", err)
	}
	var n int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM owner_managed_actor_lineage`).Scan(&n))
	if n != 0 {
		t.Fatal("automatic transition produced lineage")
	}
}

func TestOwnerDerivedLineageDoesNotBypassInterruptedChild(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	reserveChild(t, s, p, e, "running")
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, a.SwarmID)
	mustNil(t, err)
	a, err = s.BeginOwnerManagedActorOperation(ctx, a, "owner-child-reset", "reset", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "absent")
	a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh-child")
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	before := databaseRows(t, s.db)
	if _, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "blocked-first-start", "start"); err == nil {
		t.Fatal("lineage bypassed interrupted child hold")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("held start changed inventory or history")
	}
	// Явное управление owner сохраняет прежний bypass и создаёт новый прямой root.
	a, err = s.BeginOwnerManagedActorOperation(ctx, a, "explicit-owner-start", "start", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	direct, err := s.GetOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID)
	mustNil(t, err)
	if direct.Operation != "start" || direct.State != "completed" {
		t.Fatalf("direct owner controls changed: %+v", direct)
	}
	if _, err := s.GetDerivedOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("direct start misclassified derived: %v", err)
	}
}

// BUG: Старый reset root сам по себе не подтверждает создание новой инкарнации.
func TestOwnerDerivedLineageDoesNotInheritFailedResetAsFirstStart(t *testing.T) {
	ctx := context.Background()
	for _, stage := range []string{"stop", "create"} {
		t.Run(stage, func(t *testing.T) {
			s := newTestStore(t)
			a := ownerHistoryActor(t, s)
			a, err := s.BeginOwnerManagedActorOperation(ctx, a, "failed-reset", "reset", "u", func(context.Context) error { return nil })
			mustNil(t, err)
			if stage == "create" {
				a = lifecycleConfirm(t, s, a, "absent")
				a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "new-nonce")
				mustNil(t, err)
			}
			a, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "failed-task")
			mustNil(t, err)
			a, err = s.ResolveManagedActorTaskFailure(ctx, a.UID, a.Revision, a.OperationID, a.Stage, a.UPID, "stopped")
			mustNil(t, err)
			a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "fresh-recovery-start", "start")
			mustNil(t, err)
			if link, err := s.GetDerivedOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("failed reset was promoted to owner first-start provenance: %+v %v", link, err)
			}
			if stage == "stop" {
				_, err = s.db.Exec(`INSERT INTO owner_managed_actor_lineage(actor_uid,incarnation,operation_id,operation,prior_incarnation,prior_operation_id,origin_id) VALUES(?,?,?,'start',?,'failed-reset','failed-reset')`, a.UID, a.Incarnation, a.OperationID, a.Incarnation)
				if err == nil {
					t.Fatal("SQL guard accepted a first-start link without a retained reset transition")
				}
			}
		})
	}
}

func TestOwnerDerivedLineageAtomicFailures(t *testing.T) {
	ctx := context.Background()
	for _, transition := range []string{"reset", "start"} {
		for _, failure := range []string{"insert-abort", "insert-ignore", "update-ignore", "stale-revision", "outer-rollback"} {
			t.Run(transition+"/"+failure, func(t *testing.T) {
				s := newTestStore(t)
				a := ownerHistoryActor(t, s)
				a = ownerResetForLineage(t, s, a, "root")
				if transition == "start" {
					var err error
					a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh")
					mustNil(t, err)
					a = lifecycleConfirm(t, s, a, "stopped")
				}
				switch failure {
				case "insert-abort", "insert-ignore":
					action := "ABORT,'injected'"
					if failure == "insert-ignore" {
						action = "IGNORE"
					}
					_, err := s.db.Exec(`CREATE TRIGGER injected BEFORE INSERT ON owner_managed_actor_lineage BEGIN SELECT RAISE(` + action + `); END`)
					mustNil(t, err)
				case "update-ignore":
					_, err := s.db.Exec(`CREATE TRIGGER injected BEFORE UPDATE ON managed_actors BEGIN SELECT RAISE(IGNORE); END`)
					mustNil(t, err)
				}
				before := databaseRows(t, s.db)
				txErr := s.WithTx(ctx, func(ctx context.Context) error {
					expected := a.Revision
					if failure == "stale-revision" {
						expected++
					}
					var err error
					if transition == "reset" {
						_, err = s.ConfirmManagedActorReset(ctx, a.UID, expected, a.OperationID, "fresh")
					} else {
						_, err = s.BeginManagedActorOperation(ctx, a.UID, expected, "start-derived", "start")
					}
					if failure == "outer-rollback" {
						mustNil(t, err)
						return errors.New("outer rollback")
					}
					if err == nil {
						t.Fatal("failed write accepted")
					}
					return nil
				})
				if failure == "outer-rollback" {
					if txErr == nil {
						t.Fatal("outer rollback accepted")
					}
				} else {
					mustNil(t, txErr)
				}
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("failed transition leaked inventory or lineage")
				}
			})
		}
	}
}
