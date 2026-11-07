package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
)

//go:embed managed.sql
var managedSchema string

// Schema 3 is retained verbatim to validate deployed databases before ALTER.
const managedStageColumn = `stage TEXT NOT NULL DEFAULT 'reconcile' CHECK (stage IN ('reconcile', 'create', 'start', 'stop', 'delete', 'done'))`

func managedLifecycleSchema() string {
	return strings.TrimSuffix(strings.TrimSpace(managedSchema), ");") + "," + managedStageColumn + ");"
}

var managedConstraints = []struct{ name, kind, sql string }{
	{"managed_pools_live_name", "index", `CREATE UNIQUE INDEX managed_pools_live_name ON managed_pools(name) WHERE state <> 'deleted'`},
	{"managed_actors_live_name", "index", `CREATE UNIQUE INDEX managed_actors_live_name ON managed_actors(swarm_id, name) WHERE phase <> 'deleted'`},
	{"managed_actors_live_ctid", "index", `CREATE UNIQUE INDEX managed_actors_live_ctid ON managed_actors(cluster_identity, ctid) WHERE phase <> 'deleted'`},
	{"managed_swarm_delete_guard", "trigger", `CREATE TRIGGER managed_swarm_delete_guard BEFORE DELETE ON swarms WHEN EXISTS (SELECT 1 FROM managed_actors WHERE swarm_id = OLD.id AND phase <> 'deleted') BEGIN SELECT RAISE(ABORT, 'swarm has managed actors'); END`},
}

func initializeManaged(ctx context.Context, tx *sql.Tx) error {
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 2 {
		var reserved int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name GLOB 'managed_*'`).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 0 {
			return fmt.Errorf("incompatible database schema: unversioned managed inventory objects")
		}
		if _, err := tx.ExecContext(ctx, managedSchema); err != nil {
			return fmt.Errorf("extend managed inventory schema: %w", err)
		}
		for _, object := range managedConstraints {
			if _, err := tx.ExecContext(ctx, object.sql); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA user_version=3`); err != nil {
			return err
		}
		version = 3
	} else if version != 3 && version != 4 && version != 5 && version != 6 && version != 7 && version != 8 && version != 9 && version != 10 && version != 11 && version != 12 && version != 13 && version != 14 && version != 15 && version != 16 {
		return fmt.Errorf("incompatible database schema: unsupported version %d", version)
	}
	expectedSchema := managedSchema
	if version >= 4 {
		expectedSchema = managedLifecycleSchema()
	}
	if version >= 7 {
		expectedSchema = managedDelegationSchema()
	}
	if version >= 10 {
		expectedSchema = managedOriginSchema()
	}
	if err := checkSchema(ctx, tx, expectedSchema); err != nil {
		return err
	}
	for _, object := range managedConstraints {
		var actual string
		if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type=? AND name=?`, object.kind, object.name).Scan(&actual); err != nil {
			return fmt.Errorf("incompatible database schema: missing managed constraint %s: %w", object.name, err)
		}
		if normalizeSchemaSQL(actual) != normalizeSchemaSQL(object.sql) {
			return fmt.Errorf("incompatible database schema: managed constraint %s differs", object.name)
		}
	}
	if version == 3 {
		for _, statement := range []string{
			`ALTER TABLE managed_actors ADD COLUMN ` + managedStageColumn,
			`UPDATE managed_actors SET stage='done' WHERE phase='deleted'`,
			`PRAGMA user_version=4`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("extend managed lifecycle schema: %w", err)
			}
		}
	}
	if version <= 4 {
		for _, statement := range []string{
			managedConnectionsSchema,
			`PRAGMA user_version=5`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("extend managed connection catalog: %w", err)
			}
		}
	}
	if version <= 5 {
		if err := checkSchema(ctx, tx, managedConnectionsSchema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE managed_connections ADD COLUMN runtime_config TEXT NOT NULL DEFAULT ''; PRAGMA user_version=6`); err != nil {
			return fmt.Errorf("extend managed connection runtime: %w", err)
		}
	}
	if err := checkSchema(ctx, tx, managedConnectionsRuntimeSchema()); err != nil {
		return err
	}
	if version <= 6 {
		for _, column := range managedDelegationColumns {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE managed_actors ADD COLUMN `+column); err != nil {
				return fmt.Errorf("extend managed delegation: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA user_version=7`); err != nil {
			return err
		}
	}
	if version >= 10 {
		return checkSchema(ctx, tx, managedOriginSchema())
	}
	return checkSchema(ctx, tx, managedDelegationSchema())
}

var managedDelegationColumns = []string{
	`role TEXT NOT NULL DEFAULT 'worker' CHECK (role IN ('worker', 'master'))`,
	`delegator_id TEXT NOT NULL DEFAULT ''`,
	`parent_uid TEXT NOT NULL DEFAULT ''`,
}

func managedDelegationSchema() string {
	return strings.TrimSuffix(strings.TrimSpace(managedLifecycleSchema()), ");") + "," + strings.Join(managedDelegationColumns, ",") + ");"
}

func managedConnectionsRuntimeSchema() string {
	return strings.TrimSuffix(strings.TrimSpace(managedConnectionsSchema), ")") + ",runtime_config TEXT NOT NULL DEFAULT '')"
}

// managedConnectionsSchema — каталог подключений Proxmox панели (#118).
// Секрет хранится рядом с полями подключения, но читается только кодом
// создания PVE-клиента.
const managedConnectionsSchema = `CREATE TABLE managed_connections (
	uid TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	endpoint TEXT NOT NULL,
	token_id TEXT NOT NULL,
	token_secret TEXT NOT NULL,
	ca_pem TEXT NOT NULL,
	node TEXT NOT NULL,
	resource_pool TEXT NOT NULL,
	storage TEXT NOT NULL,
	network TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
)`
