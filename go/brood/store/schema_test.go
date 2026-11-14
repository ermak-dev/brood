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
// и версией schemaVersion; совпадающая версия проверяется целиком; версия
// вне цепочки миграций или база без версии отвергается без изменения файла
// (путь от прежней версии проверяет migration_test.go, #274).
// IMPLEMENTATION: initializeSchema читает PRAGMA user_version и обходит
// schemaModules; модули схемы не знают о версиях.
// RESULT: IMPLEMENTED, без migration ledger: версия хранится в PRAGMA user_version.
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
	want := []string{"command_project_details", "coordination_claims", "gitlab_approvals", "gitlab_bindings", "gitlab_grants", "gitlab_operations", "managed_actors", "managed_connections", "managed_pools", "pipeline_sources", "project_capacity", "project_git_credential_refs", "project_git_credentials", "project_members", "project_repositories", "project_secret_files", "project_secret_revisions", "project_secret_vars", "projects", "sessions", "sqlite_sequence", "swarm_members", "swarm_requests", "swarm_role_bindings", "swarm_steps", "swarms", "telegram_outbox", "user_telegram_links", "users", "workflow_definitions", "workflow_revision_counters"}
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

// TestOpenRejectsForeignSchemaVersion: версия без цепочки миграций (до 26,
// #274) и версия новее релиза отвергаются, а не обновляются. Отказ обязан
// назвать обе версии и не тронуть файл: сравниваем снимок sqlite_master и
// user_version.
func TestOpenRejectsForeignSchemaVersion(t *testing.T) {
	for _, version := range []int{0, 7, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, schemaVersion + 1, 99} {
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
	want := []string{"users", "projects", "project_members", "swarms", "sessions", "swarm_members", "project_capacity", "managed_pools", "managed_actors", "managed_connections", "coordination_claims", "command_project_details", "project_secret_revisions", "project_secret_files", "project_secret_vars", "project_repositories", "project_git_credentials", "project_git_credential_refs", "swarm_steps", "workflow_definitions", "workflow_revision_counters", "pipeline_sources", "swarm_role_bindings", "swarm_requests", "user_telegram_links", "telegram_outbox", "gitlab_bindings", "gitlab_grants", "gitlab_operations", "gitlab_approvals"}
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

// FEATURE: Модель «актор под узел» удалена из схемы (#223).
// SPEC: Узел называет роль, рой не создаёт актора под узел (#204, #208).
// Ни реестр модулей, ни созданная им база не несут таблицы
// pipeline_preparations, её триггеров и колонок native_* у swarm_steps.
// База версии 18, где они ещё есть, отвергается с обеими версиями и без
// изменения файла; та же база, помеченная текущей версией, тоже
// отвергается: шаг с колонками native_* не совпадает с реестром.
// IMPLEMENTATION: Модуль pipeline-preparation и колонки убраны из
// schemaModules, schemaVersion поднята.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestSchemaWithoutNodeActorModel(t *testing.T) {
	for _, module := range schemaModules {
		ddl := module.create()
		for _, dead := range []string{"pipeline_preparation", "native_"} {
			if strings.Contains(ddl, dead) {
				t.Errorf("module %s DDL still declares %s", module.id, dead)
			}
		}
	}
	ctx := context.Background()
	for _, version := range []int{18, schemaVersion} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db.sqlite3")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			var leftovers int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name LIKE 'pipeline_preparations%' OR sql LIKE '%pipeline_preparations%' OR sql LIKE '%native\_%' ESCAPE '\'`).Scan(&leftovers); err != nil || leftovers != 0 {
				t.Fatalf("fresh database keeps %d node actor objects, error = %v", leftovers, err)
			}
			// База прежнего выпуска: колонки шага из схемы 18 и, в сокращённом
			// виде, таблица подготовки с одним из её триггеров.
			if _, err := s.db.ExecContext(ctx, `
				ALTER TABLE swarm_steps ADD COLUMN native_actor_name TEXT;
				ALTER TABLE swarm_steps ADD COLUMN native_template_name TEXT;
				ALTER TABLE swarm_steps ADD COLUMN native_template_uid TEXT;
				ALTER TABLE swarm_steps ADD COLUMN native_template_digest TEXT;
				CREATE TABLE pipeline_preparations (
				    swarm_id TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
				    sequence INTEGER NOT NULL CHECK (sequence > 0),
				    actor_uid TEXT NOT NULL REFERENCES managed_actors(uid),
				    state TEXT NOT NULL CHECK (state IN ('reserved','unconfirmed')),
				    PRIMARY KEY (swarm_id, sequence)
				);
				CREATE TRIGGER pipeline_preparations_swarm_no_delete BEFORE DELETE ON swarms WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE swarm_id=OLD.id) BEGIN SELECT RAISE(ABORT, 'pipeline preparation remains unresolved'); END`); err != nil {
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
			want := fmt.Sprintf("found %d, want %d", version, schemaVersion)
			if version == schemaVersion {
				want = "table swarm_steps differs from current schema"
			}
			if err == nil || !strings.Contains(err.Error(), "incompatible database schema") || !strings.Contains(err.Error(), want) {
				t.Fatalf("Open error = %v, want incompatible database schema naming %q", err, want)
			}
			if err := assertDatabaseUnchanged(t, path, version, before); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestOpenRejectsDamagedTriggers: подменённый или отсутствующий триггер
// целостности ловится при открытии базы текущей версии.
func TestOpenRejectsDamagedTriggers(t *testing.T) {
	for name, change := range map[string]string{
		"missing trigger":                       `DROP TRIGGER pipeline_sources_immutable`,
		"missing step report fence":             `DROP TRIGGER swarm_steps_report_fence`,
		"missing pass fence":                    `DROP TRIGGER swarm_steps_pass_fence`,
		"different trigger":                     `DROP TRIGGER swarm_graph_immutable; CREATE TRIGGER swarm_graph_immutable BEFORE UPDATE ON swarms BEGIN SELECT RAISE(ABORT, 'other'); END`,
		"missing index":                         `DROP INDEX swarms_live_name`,
		"missing live name index for managed":   `DROP INDEX managed_pools_live_name`,
		"missing outbox fence":                  `DROP TRIGGER telegram_outbox_fence`,
		"missing unconfirmed outbox fence":      `DROP TRIGGER telegram_outbox_unconfirmed`,
		"missing outbox replace fence":          `DROP TRIGGER telegram_outbox_no_replace`,
		"missing outbox delete fence":           `DROP TRIGGER telegram_outbox_no_delete`,
		"missing outbox attempt fence":          `DROP TRIGGER telegram_outbox_attempts`,
		"missing telegram session revocation":   `DROP TRIGGER sessions_telegram_revoke`,
		"different telegram session revocation": `DROP TRIGGER sessions_telegram_revoke; CREATE TRIGGER sessions_telegram_revoke BEFORE DELETE ON sessions BEGIN SELECT 1; END`,
		"missing outbox queue index":            `DROP INDEX telegram_outbox_pending`,
		"missing outbox user index":             `DROP INDEX telegram_outbox_user`,
		"missing outbox insert fence":           `DROP TRIGGER telegram_outbox_insert`,
		"missing outbox id fence":               `DROP TRIGGER telegram_outbox_fresh_id`,
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
