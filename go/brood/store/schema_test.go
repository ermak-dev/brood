package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Инициализация текущей схемы и точное расширение ACL.
// SPEC: Чистая база содержит девять таблиц и retry; повторное открытие сохраняет
// данные и схему; неизвестная база отвергается без обновления.
// IMPLEMENTATION: Встроенные определения и ACL-расширение в BEGIN IMMEDIATE.
// RESULT: IMPLEMENTED — без импорта и migration ledger; определения проверяются.
// STATUS: DONE
func TestOpenCurrentSchema(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
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
	want := []string{"command_project_details", "coordination_claims", "managed_actors", "managed_connections", "managed_pools", "pipeline_runs", "pipeline_steps", "project_capacity", "project_members", "project_repositories", "project_secret_files", "project_secret_revisions", "project_secret_vars", "projects", "sessions", "swarm_members", "swarms", "users", "workflow_definitions"}
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

// TestOpenPreviousCurrentSchema воспроизводит схему уже обновлённой базы:
// retry добавлен ALTER, а служебная таблица осталась и не меняется.
func TestOpenPreviousCurrentSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite3")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		ALTER TABLE swarms DROP COLUMN retry;
		ALTER TABLE swarms ADD COLUMN retry TEXT NOT NULL DEFAULT '' CHECK (retry IN ('', 'create', 'delete'));
		CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL);
		INSERT INTO schema_migrations VALUES ('historical-entry', 'now')`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, s.db)
	s.Close()
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if after := schemaSnapshot(t, reopened.db); !reflect.DeepEqual(after, before) {
		t.Errorf("reopening changed current schema: %v => %v", before, after)
	}
	var entries int
	if err := reopened.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&entries); err != nil || entries != 1 {
		t.Errorf("historical ledger changed: entries = %d, error = %v", entries, err)
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
