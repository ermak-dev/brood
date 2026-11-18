package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// FEATURE: FULL requested marker ограждает каждую автоматическую physical stage.
// SPEC: Nested/stale/unknown request отказывает, ошибка откатывает запись;
// restart не восстанавливает unsent marker, обычная managed durability не меняется.
// IMPLEMENTATION: Existing inventory marker, FULL commit и revision CAS без нового ledger.
// RESULT: IMPLEMENTED — ошибки атомарны, unknown/restart не восстанавливают unsent stage.
// STATUS: DONE
func TestEphemeralLifecycleStageFailuresAreAtomic(t *testing.T) {
	for _, kind := range []string{"start", "cleanup"} {
		for _, failure := range []string{"nil", "denied", "cancel-before", "cancel-callback", "tx", "full", "stale", "inc", "op", "stage", "upid", "unknown", "ignore-update", "abort-update", "after-update"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				s, intent := lifecycleFixture(t, kind)
				a, err := beginLifecycle(s, ctx, intent.ID, kind, childAllow)
				mustNil(t, err)
				inc, op, stage, rev := a.Incarnation, a.OperationID, a.Stage, a.Revision
				auth := SwarmChildAuthorize(childAllow)
				switch failure {
				case "nil":
					auth = nil
				case "denied":
					auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
				case "cancel-before", "cancel-callback":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					if failure == "cancel-before" {
						cancel()
					} else {
						auth = func(context.Context, SwarmChildAuthorization) error { cancel(); return nil }
					}
				case "stale":
					rev--
				case "inc":
					inc++
				case "op":
					op = "other"
				case "stage":
					stage = "create"
				case "upid":
					a, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "retained-task")
					mustNil(t, err)
					rev = a.Revision
				case "unknown":
					a.Phase = "provisioning"
					if kind == "cleanup" {
						a.Phase = "deleting"
					}
					a.Error = ""
					a, err = s.UpdateManagedActor(ctx, a, a.Revision)
					mustNil(t, err)
					rev = a.Revision
				case "ignore-update", "abort-update", "after-update":
					timing, result := "BEFORE", "RAISE(ABORT,'injected update failure')"
					if failure == "ignore-update" {
						result = "RAISE(IGNORE)"
					}
					if failure == "after-update" {
						timing = "AFTER"
					}
					_, err = s.db.Exec(`CREATE TRIGGER fail_stage ` + timing + ` UPDATE ON managed_actors BEGIN SELECT ` + result + `; END`)
					mustNil(t, err)
				}
				before := databaseRows(t, s.db)
				reserve := func(ctx context.Context) {
					t.Helper()
					out, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, inc, op, stage, rev, auth)
					if err == nil || out != (ManagedActor{}) {
						t.Fatal("failed stage returned permission")
					}
					if (failure == "tx" || failure == "full") && !errors.Is(err, ErrDurableTransactionNested) {
						t.Fatalf("wrong nested error: %v", err)
					}
				}
				if failure == "tx" || failure == "full" {
					run := s.WithTx
					if failure == "full" {
						run = s.WithDurableTx
					}
					mustNil(t, run(ctx, func(ctx context.Context) error { reserve(ctx); return nil }))
				} else {
					reserve(ctx)
				}
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("failed stage changed rows")
				}
			})
		}
	}
}

func TestEphemeralLifecycleRequestedSurvivesReopenWithoutRearm(t *testing.T) {
	for _, kind := range []string{"start", "cleanup"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, intent := lifecycleFixture(t, kind)
			a, err := beginLifecycle(s, ctx, intent.ID, kind, childAllow)
			mustNil(t, err)
			a, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, a.Stage, a.Revision, childAllow)
			mustNil(t, err)
			var path string
			mustNil(t, s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path))
			mustNil(t, s.Close())
			reopened, err := Open(ctx, path)
			mustNil(t, err)
			defer reopened.Close()
			before := databaseRows(t, reopened.db)
			history, err := beginLifecycle(reopened, ctx, intent.ID, kind, childAllow)
			mustNil(t, err)
			if history != a {
				t.Fatal("restart replaced requested operation")
			}
			mustNil(t, reopened.CheckEphemeralChildLifecycle(ctx, a.UID, 1, a.OperationID, childAllow))
			if _, err := reopened.ReserveEphemeralChildLifecycleStage(ctx, a.UID, 1, a.OperationID, a.Stage, a.Revision, childAllow); err == nil {
				t.Fatal("restart rearmed unknown submission")
			}
			if !reflect.DeepEqual(before, databaseRows(t, reopened.db)) {
				t.Fatal("history/check/restart modified requested marker")
			}
			mustNil(t, reopened.WithTx(ctx, func(ctx context.Context) error {
				var mode int
				mustNil(t, reopened.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
				if mode != 1 {
					t.Fatal("ordinary managed transaction no longer NORMAL")
				}
				return nil
			}))
		})
	}
}

func TestEphemeralLifecycleSQLInsertRequiresExactInitialOperation(t *testing.T) {
	for _, kind := range []string{"start", "cleanup"} {
		for _, change := range []string{"unbegun", "wrong-op", "wrong-kind", "inc", "phase", "stage", "upid", "class", "parent", "swarm", "namespace", "pool", "origin", "template", "source"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				s, intent := lifecycleFixture(t, kind)
				a, err := s.GetManagedActor(ctx, "isolated")
				mustNil(t, err)
				operation := "start"
				if kind == "cleanup" {
					operation = "delete"
				}
				if change != "unbegun" {
					a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "private-initial-op", operation)
					mustNil(t, err)
					if kind == "start" {
						a.Phase = "stopped"
						a, err = s.UpdateManagedActor(ctx, a, a.Revision)
						mustNil(t, err)
					}
				}
				recordedKind, op := kind, a.OperationID
				switch change {
				case "wrong-op":
					op = "other"
				case "wrong-kind":
					if kind == "start" {
						recordedKind = "cleanup"
					} else {
						recordedKind = "start"
					}
				case "inc":
					setActor(t, s, a.UID, "incarnation=2")
				case "phase":
					setActor(t, s, a.UID, "phase='error'")
				case "stage":
					setActor(t, s, a.UID, "stage='done'")
				case "upid":
					setActor(t, s, a.UID, "upid='task'")
				case "class":
					_, err = s.db.Exec(`DROP TRIGGER managed_actor_class_update_guard`)
					mustNil(t, err)
					setActor(t, s, a.UID, "actor_class='persistent'")
				case "parent":
					setActor(t, s, a.UID, "parent_uid='b'")
				case "swarm":
					setActor(t, s, a.UID, "swarm_id='two'")
				case "namespace":
					setActor(t, s, a.UID, "atespace_uid='other'")
				case "pool": // Повреждение intent моделируется только после снятия его SQL-ограды.
					_, err = s.db.Exec(`DROP TRIGGER ephemeral_child_intent_update_guard; UPDATE ephemeral_child_intents SET pool_uid='other' WHERE id=?`, intent.ID)
					mustNil(t, err)
				case "origin":
					setActor(t, s, a.UID, "origin='https://other.example/repo.git'")
				case "template":
					setActor(t, s, a.UID, "template='other'")
				case "source":
					setActor(t, s, a.UID, "source_fingerprint='other'")
				}
				before := databaseRows(t, s.db)
				if _, err := s.db.Exec(`INSERT INTO ephemeral_child_lifecycles(intent_id,kind,operation_id,created_at) VALUES(?,?,?,'now')`, intent.ID, recordedKind, op); err == nil {
					t.Fatal("raw SQL fabricated provenance from incompatible operation")
				}
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("SQL refusal changed rows")
				}
			})
		}
	}
}
