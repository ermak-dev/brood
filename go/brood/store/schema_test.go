package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// FEATURE: Инициализация полной схемы по единому списку модулей.
// SPEC: Пустая база (версия 0 без объектов) создаётся со всеми модулями
// и версией schemaVersion; совпадающая версия проверяется целиком; чужая
// версия или база без версии отвергается без изменения файла.
// IMPLEMENTATION: initializeSchema читает PRAGMA user_version один раз
// и обходит schemaModules; модули схемы не знают о версиях.
// RESULT: IMPLEMENTED — без migration ledger; определения проверяются.
// STATUS: DONE
func TestOpenCurrentSchema(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, error = %v", version, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"command_project_details", "coordination_claims", "managed_actors", "managed_connections", "managed_pools", "pipeline_preparations", "pipeline_sources", "project_capacity", "project_members", "project_repositories", "project_secret_files", "project_secret_revisions", "project_secret_vars", "projects", "sessions", "swarm_members", "swarm_requests", "swarm_role_bindings", "swarm_steps", "swarms", "users", "workflow_definitions", "workflow_revision_counters"}
	if !reflect.DeepEqual(tables, want) {
		t.Fatalf("tables = %v, want %v (no migration ledger)", tables, want)
	}
	rows.Close()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO users VALUES ('u', 'alice', NULL, 'hash', 0, 0, 'now', 'now');
		INSERT INTO projects VALUES ('p', 'project', 'now');
		INSERT INTO swarms (id, project_id, owner_id, name, atespace_name, created_at, updated_at)
		VALUES ('s', 'p', 'u', 'swarm', 'swarm-s', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}
	var retry string
	if err := s.db.QueryRowContext(ctx, `SELECT retry FROM swarms WHERE id = 's'`).Scan(&retry); err != nil || retry != "" {
		t.Fatalf("retry default = %q, error = %v", retry, err)
	}
	for _, value := range []any{nil, "invalid"} {
		if _, err := s.db.ExecContext(ctx, `UPDATE swarms SET retry = ? WHERE id = 's'`, value); err == nil {
			t.Fatalf("invalid retry %v accepted", value)
		}
	}
}

func TestOpenIdempotentOnReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite3")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.CreateUser(ctx, User{ID: "u", Username: "alice", PasswordHash: "hash"}); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, first.db)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reopened, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reopened.GetUserByUsername(ctx, "alice"); err != nil {
			t.Fatal(err)
		}
		if after := schemaSnapshot(t, reopened.db); !reflect.DeepEqual(after, before) {
			t.Errorf("reopening changed schema: %v => %v", before, after)
		}
		reopened.Close()
	}
}

// TestOpenRejectsForeignSchemaVersion: миграций нет, старая база не
// обновляется, а отвергается. Отказ обязан назвать обе версии и не
// тронуть файл: сравниваем снимок sqlite_master и user_version.
func TestOpenRejectsForeignSchemaVersion(t *testing.T) {
	for _, version := range []int{0, 7, 15, 16, 17, 19, 99} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db.sqlite3")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO users VALUES ('u','alice',NULL,'hash',0,0,'now','now')`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, version)); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, s.db)
			s.Close()
			reopened, err := Open(ctx, path)
			if reopened != nil {
				reopened.Close()
			}
			if err == nil {
				t.Fatal("foreign schema version accepted")
			}
			if !strings.Contains(err.Error(), "incompatible database schema") ||
				!strings.Contains(err.Error(), fmt.Sprintf("found %d", version)) ||
				!strings.Contains(err.Error(), fmt.Sprintf("want %d", schemaVersion)) {
				t.Fatalf("Open error = %q, want incompatible database schema naming found %d and want %d", err, version, schemaVersion)
			}
			if err := assertDatabaseUnchanged(t, path, version, before); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestOpenRejectsUnversionedSchema: база с объектами, но без версии,
// не является пустой и должна отвергаться без создания чего-либо.
func TestOpenRejectsUnversionedSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite3")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY); INSERT INTO users VALUES ('u')`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, db)
	db.Close()
	if s, err := Open(ctx, path); err == nil {
		s.Close()
		t.Fatal("unversioned non-empty database accepted")
	} else if !strings.Contains(err.Error(), "incompatible database schema") {
		t.Fatal(err)
	}
	if err := assertDatabaseUnchanged(t, path, 0, before); err != nil {
		t.Fatal(err)
	}
}

// TestSchemaModulesCoverExpectedTables: тест перечисляет ожидаемые таблицы;
// модуль, забытый в schemaModules, не создаст своей таблицы, и пустая база
// откроется без неё — расхождение списков поймает именно этот тест.
func TestSchemaModulesCoverExpectedTables(t *testing.T) {
	if len(schemaModules) == 0 {
		t.Fatal("schemaModules is empty: no module DDL would run")
	}
	seen := map[string]bool{}
	for _, module := range schemaModules {
		create := module.create()
		if create == "" {
			t.Fatalf("module %s has no create DDL", module.id)
		}
		for _, statement := range strings.Split(create, ";") {
			fields := strings.Fields(statement)
			if len(fields) < 3 || !strings.HasPrefix(fields[0], "CREATE") {
				continue
			}
			if fields[1] != "TABLE" {
				continue
			}
			table := fields[2]
			if seen[table] {
				t.Fatalf("table %s declared by more than one module", table)
			}
			seen[table] = true
		}
	}
	want := []string{"users", "projects", "project_members", "swarms", "sessions", "swarm_members", "project_capacity", "managed_pools", "managed_actors", "managed_connections", "coordination_claims", "command_project_details", "project_secret_revisions", "project_secret_files", "project_secret_vars", "project_repositories", "swarm_steps", "workflow_definitions", "workflow_revision_counters", "pipeline_sources", "pipeline_preparations", "swarm_role_bindings", "swarm_requests"}
	for _, table := range want {
		if !seen[table] {
			t.Errorf("table %s is missing from schemaModules DDL", table)
		}
	}
	for table := range seen {
		if !slices.Contains(want, table) {
			t.Errorf("schemaModules declares unexpected table %s", table)
		}
	}
}

// TestOpenRejectsDamagedTriggers: подменённый или отсутствующий триггер
// целостности ловится при открытии базы текущей версии.
func TestOpenRejectsDamagedTriggers(t *testing.T) {
	for name, change := range map[string]string{
		"missing trigger":                     `DROP TRIGGER pipeline_sources_immutable`,
		"different trigger":                   `DROP TRIGGER swarm_graph_immutable; CREATE TRIGGER swarm_graph_immutable BEFORE UPDATE ON swarms BEGIN SELECT RAISE(ABORT, 'other'); END`,
		"missing preparation trigger":         `DROP TRIGGER pipeline_preparations_actor_fence`,
		"missing index":                       `DROP INDEX swarms_live_name`,
		"missing live name index for managed": `DROP INDEX managed_pools_live_name`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db.sqlite3")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, change); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, s.db)
			s.Close()
			reopened, err := Open(ctx, path)
			if reopened != nil {
				reopened.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "incompatible database schema") {
				t.Errorf("Open error = %v, want incompatible database schema", err)
			}
			if err := assertDatabaseUnchanged(t, path, schemaVersion, before); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenRejectsIncompatibleSchemaWithoutChanges(t *testing.T) {
	for name, change := range map[string]string{
		"old schema without retry": `ALTER TABLE swarms DROP COLUMN retry`,
		"missing table":            `DROP TABLE sessions`,
		"different default":        `PRAGMA writable_schema = ON; UPDATE sqlite_master SET sql = replace(sql, '''provisioning''', '''provision ing''') WHERE name = 'swarms'; PRAGMA writable_schema = OFF;`,
		"missing constraint":       `DROP TABLE sessions; CREATE TABLE sessions (token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT, data BLOB)`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db.sqlite3")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, change); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, s.db)
			s.Close()
			reopened, err := Open(ctx, path)
			if reopened != nil {
				reopened.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "incompatible database schema") {
				t.Errorf("Open error = %v, want incompatible database schema", err)
			}
			raw, err := sql.Open("sqlite", dsn(path))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if after := schemaSnapshot(t, raw); !reflect.DeepEqual(after, before) {
				t.Errorf("failed Open changed schema: %v => %v", before, after)
			}
		})
	}
}

// assertDatabaseUnchanged сверяет схему и версию базы с снимком до
// отказа: отвергнутое открытие не имеет права менять файл.
func assertDatabaseUnchanged(t *testing.T, path string, version int, before []string) error {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if after := schemaSnapshot(t, db); !reflect.DeepEqual(after, before) {
		return fmt.Errorf("refused Open changed schema: %v => %v", before, after)
	}
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil || current != version {
		return fmt.Errorf("refused Open changed version: %d, error = %v", current, err)
	}
	return nil
}

func schemaSnapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			t.Fatal(err)
		}
		result = append(result, definition)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
