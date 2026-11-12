package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// FEATURE: Schema migrations instead of recreating the installation (#274).
// SPEC: an existing database of version N < schemaVersion is brought to the
// current schema by the ordered steps N→N+1→…, each in its own transaction
// that records its version; the last step verifies the result against the
// DDL registry before it commits. A fresh database is created at the current
// version directly. A newer, unknown or unchained version is refused before
// any change; a failing step rolls back to the version before it.
// IMPLEMENTATION: schemaMigrations, migrationPlan and prepareSchema in
// migration.go; Open and VerifyDatabaseSchema share them.
// STATUS: DONE

func TestMigrationPlanWalksTheChainOrRefusesBeforeAnyChange(t *testing.T) {
	steps := []schemaMigration{{from: 24, sql: "SELECT 24"}, {from: 25, sql: "SELECT 25"}, {from: 26, sql: "SELECT 26"}}
	plan, err := migrationPlan(24, 27, steps)
	if err != nil || len(plan) != 3 || plan[0].from != 24 || plan[1].from != 25 || plan[2].from != 26 {
		t.Fatalf("plan 24->27 = %+v, %v", plan, err)
	}
	if plan, err := migrationPlan(27, 27, steps); err != nil || len(plan) != 0 {
		t.Fatalf("current version plans %+v, %v", plan, err)
	}
	for name, tc := range map[string]struct {
		found int
		steps []schemaMigration
		want  string
	}{
		"newer database":  {28, steps, "incompatible database schema: found 28, want 27; the database is newer than this release"},
		"unknown version": {15, steps, "incompatible database schema: found 15, want 27; no migration from version 15"},
		"empty version":   {0, steps, "incompatible database schema: found 0, want 27; no migration from version 0"},
		"gap in chain":    {24, []schemaMigration{steps[0], steps[2]}, "incompatible database schema: found 24, want 27; no migration from version 25"},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := migrationPlan(tc.found, 27, tc.steps)
			if err == nil || err.Error() != tc.want || !errors.Is(err, errIncompatibleSchema) || plan != nil {
				t.Fatalf("plan = %+v, error = %v, want %q", plan, err, tc.want)
			}
		})
	}
}

// TestSchemaMigrationsFormChainToCurrentVersion: реестр задаёт непрерывную
// цепочка шагов по одной версии, последний ведёт в schemaVersion, и у каждой
// исходной версии есть замороженная база, на которой шаг проверяется.
func TestSchemaMigrationsFormChainToCurrentVersion(t *testing.T) {
	if len(schemaMigrations) == 0 {
		t.Fatal("no migrations: a deployed database of the previous version cannot open")
	}
	for i, step := range schemaMigrations {
		if step.sql == "" && step.data == nil {
			t.Errorf("migration from %d does nothing", step.from)
		}
		if i > 0 && step.from != schemaMigrations[i-1].from+1 {
			t.Errorf("migration from %d follows %d: the chain must be ordered without gaps", step.from, schemaMigrations[i-1].from)
		}
		if _, err := os.Stat(schemaFixturePath(step.from)); err != nil {
			t.Errorf("migration from %d has no frozen database %s: %v", step.from, schemaFixturePath(step.from), err)
		}
	}
	if last := schemaMigrations[len(schemaMigrations)-1]; last.from+1 != schemaVersion {
		t.Fatalf("last migration leads to %d, want schemaVersion %d", last.from+1, schemaVersion)
	}
}

// TestMigrateV26DatabaseToCurrent: база версии 26, записанная кодом той же
// версии, открывается текущим кодом, получает индекс swarm_requests(swarm_id)
// и совпадает со свежей базой объект в объект; строки не меняются, внешние
// ключи, триггеры и WAL работают после миграции.
func TestMigrateV26DatabaseToCurrent(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 26)
	raw := rawDatabase(t, path)
	before := databaseRows(t, raw)
	raw.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v26 database refused: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 27 {
		t.Fatalf("migrated version = %d, %v; want 27", version, err)
	}
	fresh := newTestStore(t)
	if got, want := schemaObjects(t, s.db), schemaObjects(t, fresh.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated schema differs from a fresh one:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(strings.Join(schemaObjects(t, s.db), "\n"), "CREATE INDEX swarm_requests_swarm ON swarm_requests(swarm_id)") {
		t.Fatal("migration did not create swarm_requests_swarm")
	}
	if after := databaseRows(t, s.db); !reflect.DeepEqual(after, before) {
		t.Fatalf("migration changed rows:\n got %v\nwant %v", after, before)
	}
	requireIntact(t, s.db)
	var foreignKeys int
	var journal string
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys after migration = %d, %v", foreignKeys, err)
	}
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal_mode after migration = %q, %v", journal, err)
	}
	if _, err := s.db.Exec(`INSERT INTO swarm_members VALUES ('missing-swarm', 'u', 'owner')`); !IsForeignKeyViolation(err) {
		t.Fatalf("foreign key not enforced after migration: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM swarm_requests`); err == nil || !strings.Contains(err.Error(), "swarm request is permanent") {
		t.Fatalf("swarm_requests_no_delete trigger did not fire after migration: %v", err)
	}
	// Мигрированная база рабочая: сводка списка роёв читает те же строки.
	summaries, err := s.SwarmSummaries(ctx, []string{"s"})
	if err != nil || summaries["s"].OpenRequests != 1 || summaries["s"].UndeliveredAnswers != 1 {
		t.Fatalf("summary over migrated rows = %+v, %v", summaries, err)
	}
}

// TestMigrateEveryFrozenVersionToCurrent: каждая поддержанная версия доходит
// цепочкой до текущей схемы, равной свежей базе, с теми же строками в
// таблицах обеих версий.
func TestMigrateEveryFrozenVersionToCurrent(t *testing.T) {
	ctx := context.Background()
	fresh := newTestStore(t)
	want := schemaObjects(t, fresh.db)
	for _, version := range frozenSchemaVersions(t) {
		if version == schemaVersion {
			continue
		}
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := loadSchemaFixture(t, version)
			raw := rawDatabase(t, path)
			requireIntact(t, raw)
			counts := map[string]int{}
			for table, rows := range databaseRows(t, raw) {
				counts[table] = len(rows)
			}
			raw.Close()
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatalf("v%d database refused: %v", version, err)
			}
			defer s.Close()
			if got := schemaObjects(t, s.db); !reflect.DeepEqual(got, want) {
				t.Fatalf("v%d migrated schema differs from a fresh one:\n got %q\nwant %q", version, got, want)
			}
			for table, rows := range databaseRows(t, s.db) {
				if count, ok := counts[table]; ok && count != len(rows) {
					t.Errorf("table %s has %d rows after migration, %d before", table, len(rows), count)
				}
			}
			requireIntact(t, s.db)
		})
	}
}

func TestReopeningMigratedDatabaseIsNoOp(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 26)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	objects, rows := schemaObjects(t, s.db), databaseRows(t, s.db)
	s.Close()
	for range 2 {
		reopened, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("migrated database refused on reopen: %v", err)
		}
		var version int
		if err := reopened.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
			t.Fatalf("reopened version = %d, %v", version, err)
		}
		if got := schemaObjects(t, reopened.db); !reflect.DeepEqual(got, objects) {
			t.Fatalf("reopen changed schema:\n got %q\nwant %q", got, objects)
		}
		if got := databaseRows(t, reopened.db); !reflect.DeepEqual(got, rows) {
			t.Fatal("reopen changed rows")
		}
		reopened.Close()
	}
}

// openedConn: закреплённое соединение базы с прагмами Open, как у
// initializeSchema.
func openedConn(t *testing.T, path string) (*sql.DB, *sql.Conn) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return db, conn
}

// TestFailedMigrationStepKeepsThePreviousVersion: шаг, упавший в SQL, в
// Go-переносе данных или оставивший нарушение внешнего ключа, откатывается
// целиком: версия 26, объекты и строки прежние, внешние ключи соединения
// снова включены.
func TestFailedMigrationStepKeepsThePreviousVersion(t *testing.T) {
	failure := errors.New("synthetic data transfer failure")
	for name, tc := range map[string]struct {
		step schemaMigration
		want string
	}{
		"sql":          {schemaMigration{from: 26, sql: swarmRequestSwarm + "; INSERT INTO no_such_table VALUES (1)"}, "no such table"},
		"data":         {schemaMigration{from: 26, sql: swarmRequestSwarm, data: func(context.Context, *sql.Tx) error { return failure }}, failure.Error()},
		"foreign keys": {schemaMigration{from: 26, sql: swarmRequestSwarm + "; DELETE FROM users WHERE id = 'u'"}, "foreign key violation in table"},
		// SQL проходит, но итог не равен реестру DDL: без сверки перед фиксацией
		// база записала бы 27, которую не открывает ни новый, ни прежний образ.
		"schema": {schemaMigration{from: 26, sql: "CREATE INDEX swarm_requests_swarm ON swarm_requests(state)"}, "differs"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := loadSchemaFixture(t, 26)
			raw := rawDatabase(t, path)
			objects, rows := schemaObjects(t, raw), databaseRows(t, raw)
			raw.Close()
			db, conn := openedConn(t, path)
			err := prepareSchema(ctx, conn, 27, []schemaMigration{tc.step})
			if err == nil || !strings.Contains(err.Error(), "incompatible database schema: migration from version 26 to 27 failed") ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("failing step error = %v, want migration failure naming %q", err, tc.want)
			}
			var foreignKeys int
			if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
				t.Fatalf("foreign_keys after a failed step = %d, %v", foreignKeys, err)
			}
			conn.Close()
			db.Close()
			if err := assertDatabaseUnchanged(t, path, 26, schemaSnapshotOf(t, path)); err != nil {
				t.Fatal(err)
			}
			after := rawDatabase(t, path)
			if got := schemaObjects(t, after); !reflect.DeepEqual(got, objects) {
				t.Fatalf("failed step changed schema:\n got %q\nwant %q", got, objects)
			}
			if got := databaseRows(t, after); !reflect.DeepEqual(got, rows) {
				t.Fatal("failed step changed rows")
			}
		})
	}
}

func schemaSnapshotOf(t *testing.T, path string) []string {
	t.Helper()
	db := rawDatabase(t, path)
	defer db.Close()
	return schemaSnapshot(t, db)
}

// TestMigrationRecordsTheVersionAfterEachStep: шаги идут по порядку, каждый
// в своей транзакции; упавший второй шаг не откатывает первый, база остаётся
// на версии первого, а повтор продолжает с неё.
func TestMigrationRecordsTheVersionAfterEachStep(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 26)
	_, conn := openedConn(t, path)
	failing := []schemaMigration{{from: 26, sql: swarmRequestSwarm}, {from: 27, sql: "CREATE TABLE migration_probe (x); INSERT INTO no_such_table VALUES (1)"}}
	if err := prepareSchema(ctx, conn, 28, failing); err == nil ||
		!strings.Contains(err.Error(), "migration from version 27 to 28 failed") {
		t.Fatalf("second step error = %v", err)
	}
	var version, probes, indexes int
	if err := conn.QueryRowContext(ctx, `SELECT (SELECT user_version FROM pragma_user_version),
		(SELECT count(*) FROM sqlite_master WHERE name = 'migration_probe'),
		(SELECT count(*) FROM sqlite_master WHERE name = 'swarm_requests_swarm')`).Scan(&version, &probes, &indexes); err != nil {
		t.Fatal(err)
	}
	if version != 27 || probes != 0 || indexes != 1 {
		t.Fatalf("after a failed second step: version %d, probe %d, index %d; want 27, 0, 1", version, probes, indexes)
	}
	fixed := []schemaMigration{failing[0], {from: 27, sql: "CREATE TABLE migration_probe (x)"}}
	if err := prepareSchema(ctx, conn, 28, fixed); err != nil {
		t.Fatalf("resumed chain: %v", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT (SELECT user_version FROM pragma_user_version),
		(SELECT count(*) FROM sqlite_master WHERE name = 'migration_probe')`).Scan(&version, &probes); err != nil || version != 28 || probes != 1 {
		t.Fatalf("resumed chain: version %d, probe %d, %v", version, probes, err)
	}
}

// TestOpenRefusesNewerUnknownAndBrokenChainsWithoutChanges: база новее
// релиза (старый образ на новой базе), версия без цепочки и шаг, упавший на
// чужом объекте с тем же именем, дают отказ с причиной; файл не меняется.
func TestOpenRefusesNewerUnknownAndBrokenChainsWithoutChanges(t *testing.T) {
	for name, tc := range map[string]struct {
		change  string
		version int
		want    string
	}{
		"newer":   {fmt.Sprintf("PRAGMA user_version=%d", schemaVersion+1), schemaVersion + 1, fmt.Sprintf("found %d, want %d; the database is newer than this release", schemaVersion+1, schemaVersion)},
		"unknown": {"PRAGMA user_version=25", 25, fmt.Sprintf("found 25, want %d; no migration from version 25", schemaVersion)},
		"step":    {"CREATE INDEX swarm_requests_swarm ON swarm_requests(state)", 26, "migration from version 26 to 27 failed"},
	} {
		t.Run(name, func(t *testing.T) {
			path := loadSchemaFixture(t, 26)
			raw := rawDatabase(t, path)
			if _, err := raw.Exec(tc.change); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, raw)
			rows := databaseRows(t, raw)
			raw.Close()
			s, err := Open(context.Background(), path)
			if s != nil {
				s.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "incompatible database schema: ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open error = %v, want %q", err, tc.want)
			}
			if err := assertDatabaseUnchanged(t, path, tc.version, before); err != nil {
				t.Fatal(err)
			}
			if got := databaseRows(t, rawDatabase(t, path)); !reflect.DeepEqual(got, rows) {
				t.Fatal("refused Open changed rows")
			}
		})
	}
}

// TestConcurrentOpensMigrateOnce: процессы, открывшие базу 26 одновременно,
// не применяют шаг дважды: шаг перечитывает версию под блокировкой записи.
func TestConcurrentOpensMigrateOnce(t *testing.T) {
	path := loadSchemaFixture(t, 26)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(context.Background(), path)
			if err == nil {
				s.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
	db := rawDatabase(t, path)
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version = %d, %v", version, err)
	}
}

// TestSwarmSummaryUsesTheSwarmRequestIndex: ради этого индекса шаг 26→27:
// подсчёты сводки списка роёв (#266) ищут запросы роя по индексу, а не
// просматривают таблицу запросов на каждый рой.
func TestSwarmSummaryUsesTheSwarmRequestIndex(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+swarmSummaryQuery, `["s"]`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	text := strings.Join(plan, "\n")
	if strings.Count(text, "swarm_requests_swarm") != 2 || strings.Contains(text, "SCAN r") {
		t.Fatalf("summary does not search swarm requests by swarm_requests_swarm:\n%s", text)
	}
}
