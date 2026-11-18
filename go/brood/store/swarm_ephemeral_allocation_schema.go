package store

import (
	"context"
	"database/sql"
	"strings"
)

const ephemeralChildAllocationTable = `CREATE TABLE ephemeral_child_allocations (
 intent_id TEXT PRIMARY KEY NOT NULL REFERENCES ephemeral_child_intents(id) ON DELETE RESTRICT,
 actor_uid TEXT NOT NULL UNIQUE CHECK(actor_uid<>''),
 actor_incarnation INTEGER NOT NULL CHECK(actor_incarnation=1),
 create_operation_id TEXT NOT NULL CHECK(create_operation_id<>''),
 reservation_hash TEXT NOT NULL CHECK(length(reservation_hash)=64 AND reservation_hash NOT GLOB '*[^0-9a-f]*'),
 created_at TEXT NOT NULL CHECK(created_at<>'')
)`

var ephemeralChildAllocationObjects = []struct{ name, sql string }{
	{"ephemeral_child_allocation_update_guard", `CREATE TRIGGER ephemeral_child_allocation_update_guard BEFORE UPDATE ON ephemeral_child_allocations BEGIN SELECT RAISE(ABORT,'ephemeral child allocation is immutable'); END`},
	{"ephemeral_child_allocation_delete_guard", `CREATE TRIGGER ephemeral_child_allocation_delete_guard BEFORE DELETE ON ephemeral_child_allocations BEGIN SELECT RAISE(ABORT,'ephemeral child allocation is immutable'); END`},
	{"ephemeral_child_allocation_replace_guard", `CREATE TRIGGER ephemeral_child_allocation_replace_guard BEFORE INSERT ON ephemeral_child_allocations WHEN EXISTS(SELECT 1 FROM ephemeral_child_allocations WHERE intent_id=NEW.intent_id OR actor_uid=NEW.actor_uid) BEGIN SELECT RAISE(ABORT,'ephemeral child allocation is immutable'); END`},
	{"ephemeral_child_allocation_insert_guard", `CREATE TRIGGER ephemeral_child_allocation_insert_guard BEFORE INSERT ON ephemeral_child_allocations WHEN NOT EXISTS(SELECT 1 FROM ephemeral_child_intents e JOIN managed_actors a ON a.uid=NEW.actor_uid WHERE e.id=NEW.intent_id AND a.actor_class='ephemeral' AND a.role='worker' AND a.parent_uid=e.parent_uid AND a.swarm_id=e.swarm_id AND a.atespace_uid=e.atespace_uid AND a.pool_uid=e.pool_uid AND a.origin=e.origin AND a.source_fingerprint=e.source_fingerprint AND a.template=e.template AND a.incarnation=NEW.actor_incarnation AND a.operation_id=NEW.create_operation_id AND a.operation='create' AND a.stage='create' AND a.phase='provisioning' AND a.desired='running') BEGIN SELECT RAISE(ABORT,'ephemeral child allocation scope conflicts'); END`},
}

func ephemeralChildAllocationFullSchema() string {
	statements := []string{ephemeralChildAllocationTable}
	for _, object := range ephemeralChildAllocationObjects {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

func verifyEphemeralChildAllocationSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, ephemeralChildAllocationTable); err != nil {
		return err
	}
	for _, object := range ephemeralChildAllocationObjects {
		if err := checkObject(ctx, tx, "trigger", object.name, object.sql); err != nil {
			return err
		}
	}
	return nil
}
