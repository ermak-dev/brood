package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed managed.sql
var managedSchema string

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
	} else if version != 3 {
		return fmt.Errorf("incompatible database schema: unsupported version %d", version)
	}
	if err := checkSchema(ctx, tx, managedSchema); err != nil {
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
	return nil
}
