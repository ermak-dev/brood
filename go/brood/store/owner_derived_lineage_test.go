package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// FEATURE: Явная immutable lineage owner reset через новую инкарнацию и первый start.
// SPEC: Только точный completed reset, без вывода intent из класса или отсутствия истории;
// исходный журнал и coordination holds не меняются, ошибка записи откатывает переход.
// IMPLEMENTATION: Отдельный immutable ledger и exact read, запись только из двух managed-переходов.
// RESULT: IMPLEMENTED — исходный completed reset сохраняется без новой authority и legacy inference.
// STATUS: DONE
func ownerResetForLineage(t *testing.T, s *Store, a ManagedActor, id string) ManagedActor {
	t.Helper()
	a, err := s.BeginOwnerManagedActorOperation(context.Background(), a, id, "reset", "u", func(context.Context) error { return nil })
	mustNil(t, err)
	return lifecycleConfirm(t, s, a, "absent")
}

func TestOwnerDerivedLineageResetAndStart(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := ownerHistoryActor(t, s)
	a = ownerResetForLineage(t, s, a, "owner-root")
	root, err := s.GetOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID)
	mustNil(t, err)
	a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "new-nonce")
	mustNil(t, err)
	var origin string
	mustNil(t, s.db.QueryRow(`SELECT origin_id FROM owner_managed_actor_lineage WHERE actor_uid=? AND incarnation=? AND operation_id=?`, a.UID, a.Incarnation, a.OperationID).Scan(&origin))
	if origin != root.OperationID {
		t.Fatalf("wrong root: %s", origin)
	}
	if _, err := s.GetOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exact journal semantics changed: %v", err)
	}
	a = lifecycleConfirm(t, s, a, "stopped")
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "fresh-start", "start")
	mustNil(t, err)
	mustNil(t, s.db.QueryRow(`SELECT origin_id FROM owner_managed_actor_lineage WHERE actor_uid=? AND incarnation=? AND operation_id=?`, a.UID, a.Incarnation, a.OperationID).Scan(&origin))
	if origin != root.OperationID {
		t.Fatalf("start lost root: %s", origin)
	}
	before := databaseRows(t, s.db)
	_, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, a.OperationID, "start")
	mustNil(t, err)
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("replay changed lineage")
	}
	a = lifecycleConfirm(t, s, a, "running")
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "later-stop", "stop")
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "later-start", "start")
	mustNil(t, err)
	if _, err := s.GetDerivedOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ordinary stop/start inherited old root: %v", err)
	}
}

func seedOwnerDerivedLineageFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	request := managedRequest("fixture-owner-lineage", "one", 399)
	pool, err := s.GetManagedPool(ctx, "pool")
	mustNil(t, err)
	pool.MaxActors++
	pool.Resources.CPUMillicores += request.Resources.CPUMillicores
	pool.Resources.MemoryBytes += request.Resources.MemoryBytes
	pool.Resources.DiskBytes += request.Resources.DiskBytes
	_, err = s.UpdateManagedPool(ctx, pool, pool.Revision)
	mustNil(t, err)
	a, err := s.ReserveManagedActor(ctx, request)
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	a = ownerResetForLineage(t, s, a, "fixture-owner-reset")
	a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fixture-fresh-nonce")
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	_, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "fixture-owner-derived-start", "start")
	mustNil(t, err)
}

func TestOwnerDerivedLineageOfficialFixture56(t *testing.T) {
	s, err := Open(context.Background(), loadSchemaFixture(t, 56))
	mustNil(t, err)
	defer s.Close()
	for _, id := range []string{"fixture-owner-reset", "fixture-owner-derived-start"} {
		link, err := s.GetDerivedOwnerManagedActorOperation(context.Background(), "fixture-owner-lineage", 2, id)
		mustNil(t, err)
		if link.Origin.OperationID != "fixture-owner-reset" || link.Origin.Generation != 1 || link.Origin.State != "completed" {
			t.Fatalf("fixture lost original root: %+v", link)
		}
	}
}

func TestOwnerDerivedLineageMigrationDoesNotInferLegacyReset(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 55)
	raw := rawDatabase(t, path)
	// Моделируем уже законченный schema55 reset с положительным root на старой
	// инкарнации, но без явной lineage: upgrade не должен исправлять историю.
	_, err := raw.Exec(`INSERT INTO actor_interventions VALUES('legacy-owner-reset','one','one','fixture-context-actor',1,'u','operator:u','reset','completed','legacy');
 UPDATE managed_actors SET incarnation=2,operation_id='legacy-owner-reset',operation='reset',stage='done',phase='stopped',desired='running',upid='',error='' WHERE uid='fixture-context-actor'`)
	mustNil(t, err)
	before := databaseRows(t, raw)
	before["owner_managed_actor_lineage"] = nil
	before["ephemeral_child_executor_failures"] = nil
	before["swarm_eval_bindings"] = nil
	before["swarm_eval_replays"] = nil
	mustNil(t, raw.Close())
	s, err := Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	if !reflect.DeepEqual(withEmptyPolicyReviewTables(before), databaseRows(t, s.db)) {
		t.Fatal("upgrade inferred reset link or rewrote old rows")
	}
	a, err := s.GetManagedActor(ctx, "fixture-context-actor")
	mustNil(t, err)
	_, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "legacy-first-start", "start")
	mustNil(t, err)
	if _, err := s.GetDerivedOwnerManagedActorOperation(ctx, a.UID, 2, "legacy-first-start"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lazy inference from old incarnation: %v", err)
	}
	root, err := s.GetOwnerManagedActorOperation(ctx, a.UID, 1, "legacy-owner-reset")
	mustNil(t, err)
	if root.State != "completed" {
		t.Fatalf("old root lost: %+v", root)
	}
	requireIntact(t, s.db)
}

func TestOwnerDerivedLineageMigrationPreserves55(t *testing.T) {
	path := loadSchemaFixture(t, 55)
	raw := rawDatabase(t, path)
	before := databaseRows(t, raw)
	mustNil(t, raw.Close())
	s, err := Open(context.Background(), path)
	mustNil(t, err)
	defer s.Close()
	before["owner_managed_actor_lineage"] = nil
	before["ephemeral_child_executor_failures"] = nil
	before["swarm_eval_bindings"] = nil
	before["swarm_eval_replays"] = nil
	if !reflect.DeepEqual(withEmptyPolicyReviewTables(before), databaseRows(t, s.db)) {
		t.Fatal("migration changed old rows or inferred lineage")
	}
	requireIntact(t, s.db)
}
