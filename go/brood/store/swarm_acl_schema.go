package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed swarm_members.sql
var swarmMembersSchema string

const liveSwarmNameIndex = `CREATE UNIQUE INDEX swarms_live_name ON swarms(project_id, name) WHERE state <> 'deleted'`

// initializeACL extends only the recognized base schema, within its opening transaction.
// A versioned ACL database is validated, never reconstructed from owner_id.
func initializeACL(ctx context.Context, tx *sql.Tx) error {
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 0 {
		var reserved int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE name IN ('swarm_members', 'swarms_live_name')`).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 0 {
			return fmt.Errorf("incompatible database schema: unversioned swarm ACL objects")
		}
		for _, statement := range []string{
			swarmMembersSchema,
			`INSERT INTO swarm_members (swarm_id, user_id, role) SELECT id, owner_id, 'owner' FROM swarms`,
			liveSwarmNameIndex,
			`PRAGMA user_version = 1`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("extend swarm ACL schema: %w", err)
			}
		}
	} else if version != 1 && version != 2 && version != 3 && version != 4 && version != 5 && version != 6 && version != 7 && version != 8 {
		return fmt.Errorf("incompatible database schema: unsupported version %d", version)
	}
	if err := checkSchema(ctx, tx, swarmMembersSchema); err != nil {
		return err
	}
	var index string
	if err := tx.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'swarms_live_name'`).Scan(&index); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("incompatible database schema: missing live swarm name constraint")
		}
		return err
	}
	if normalizeSchemaSQL(index) != normalizeSchemaSQL(liveSwarmNameIndex) {
		return fmt.Errorf("incompatible database schema: live swarm name constraint differs")
	}
	var orphaned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM swarms sw
		WHERE NOT EXISTS(SELECT 1 FROM swarm_members m
			WHERE m.swarm_id = sw.id AND m.user_id = sw.owner_id AND m.role = 'owner'))`).Scan(&orphaned); err != nil {
		return err
	}
	if orphaned {
		return fmt.Errorf("incompatible database schema: swarm primary owner has no owner grant")
	}
	return nil
}
