package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// lifecycleActor — актор с доказательством незавершённой задачи: как
// прежний фикстур, выставляет неизвестный UPID напрямую; данные такого
// актора переживают закрытие и открытие базы.
func lifecycleActor(ctx context.Context, t *testing.T, s *Store) ManagedActor {
	t.Helper()
	managedFixture(t, s)
	if _, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101)); err != nil {
		t.Fatal(err)
	}
	// Прямая запись, как в прежнем фикстуре схемы 3: unknown task и его
	// operation_id не создаются штатными API.
	if _, err := s.db.Exec(`UPDATE managed_actors SET upid='UPID:unknown', operation_id='old-create', phase='running' WHERE uid='a'`); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetManagedActor(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// FEATURE: Единая версия схемы и полный список DDL (#205).
// SPEC: Пустая база создаётся со всей схемой и версией
// schemaVersion; повторное открытие сохраняет задачные доказательства
// актора; повреждённая схема ловится при открытии без мутации файла.
// IMPLEMENTATION: initializeSchema читает PRAGMA user_version один раз
// и обходит schemaModules; модули схемы не знают о версиях.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestManagedLifecycleDataSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a := lifecycleActor(ctx, t, s)
	s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version %d: %v", version, err)
	}
	got, err := s.GetManagedActor(ctx, a.UID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stage != "create" || got.UPID != "UPID:unknown" || got.OperationID != "old-create" || got.Revision != a.Revision || got.OwnerNonce != a.OwnerNonce || got.Incarnation != 1 {
		t.Fatalf("reopen lost evidence: %+v", got)
	}
	// Незавершённая задача не даёт начать другую, пока не сверена.
	if _, err = s.BeginManagedActorOperation(ctx, got.UID, got.Revision, "unsafe-delete", "delete"); err == nil {
		t.Fatal("unknown task replaced")
	}
	got = lifecycleConfirm(t, s, got, "stopped")
	got = lifecycleBegin(t, s, got, "reset")
	if got.Stage != "stop" || got.OwnerNonce != a.OwnerNonce {
		t.Fatalf("reconciled reset: %+v", got)
	}
	updated := got
	s.Close()
	// Результат жизненного цикла тоже переживает переоткрытие.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err = s.GetManagedActor(ctx, a.UID); err != nil || got != updated {
		t.Fatalf("second reopen changed actor: %+v, want %+v, err %v", got, updated, err)
	}
}

// TestManagedLifecycleDamagedSchemaRefused: повреждённая схема текущей
// версии ловится при открытии, файл не меняется.
func TestManagedLifecycleDamagedSchemaRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lifecycle.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleActor(ctx, t, s)
	if _, err := s.db.Exec(`DROP INDEX managed_actors_live_ctid`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, s.db)
	s.Close()
	if st, err := Open(ctx, path); err == nil {
		st.Close()
		t.Fatal("damaged schema accepted")
	} else if !errors.Is(err, errIncompatibleSchema) {
		t.Fatal(err)
	}
	if err := assertDatabaseUnchanged(t, path, schemaVersion, before); err != nil {
		t.Fatal(err)
	}
}
