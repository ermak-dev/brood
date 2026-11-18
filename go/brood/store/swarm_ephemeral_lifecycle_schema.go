package store

import (
	"context"
	"database/sql"
	"strings"
)

// UID/incarnation выводятся из immutable allocation, inventory не является FK:
// история сохраняется после физического удаления inventory.
const ephemeralChildLifecycleTable = `CREATE TABLE ephemeral_child_lifecycles (
 intent_id TEXT NOT NULL REFERENCES ephemeral_child_allocations(intent_id) ON DELETE RESTRICT,
 kind TEXT NOT NULL CHECK(kind IN ('start','cleanup')),
 operation_id TEXT NOT NULL UNIQUE CHECK(operation_id<>''),
 created_at TEXT NOT NULL CHECK(created_at<>''),
 PRIMARY KEY(intent_id,kind)
)`

var ephemeralChildLifecycleObjects = []struct{ name, sql string }{
	{"ephemeral_child_lifecycle_insert_guard", `CREATE TRIGGER ephemeral_child_lifecycle_insert_guard BEFORE INSERT ON ephemeral_child_lifecycles WHEN
 EXISTS(SELECT 1 FROM ephemeral_child_lifecycles WHERE (intent_id=NEW.intent_id AND kind=NEW.kind) OR operation_id=NEW.operation_id)
 OR NOT EXISTS(SELECT 1 FROM ephemeral_child_allocations a JOIN ephemeral_child_intents i ON i.id=a.intent_id JOIN managed_actors m ON m.uid=a.actor_uid
 WHERE a.intent_id=NEW.intent_id AND a.actor_incarnation=1 AND m.incarnation=a.actor_incarnation AND m.actor_class='ephemeral' AND m.role='worker'
 AND m.parent_uid=i.parent_uid AND m.swarm_id=i.swarm_id AND m.atespace_uid=i.atespace_uid AND m.pool_uid=i.pool_uid AND m.template=i.template AND m.source_fingerprint=i.source_fingerprint AND m.origin=i.origin
 AND m.operation_id=NEW.operation_id AND m.operation_id<>a.create_operation_id AND m.upid=''
 AND ((NEW.kind='start' AND m.operation='start' AND m.stage='start' AND m.phase='stopped' AND m.desired='running' AND m.error='' AND EXISTS(SELECT 1 FROM ephemeral_child_create_grants g WHERE g.intent_id=a.intent_id AND g.state='reserved'))
 OR (NEW.kind='cleanup' AND m.operation='delete' AND m.stage='stop' AND m.phase='deleting' AND m.desired='deleted' AND m.error='stop not requested' AND EXISTS(SELECT 1 FROM ephemeral_child_result_seals r WHERE r.intent_id=a.intent_id))))
 BEGIN SELECT RAISE(ABORT,'ephemeral child lifecycle is immutable or not allocation-bound'); END`},
	{"ephemeral_child_lifecycle_update_guard", `CREATE TRIGGER ephemeral_child_lifecycle_update_guard BEFORE UPDATE ON ephemeral_child_lifecycles BEGIN SELECT RAISE(ABORT,'ephemeral child lifecycle is immutable'); END`},
	{"ephemeral_child_lifecycle_delete_guard", `CREATE TRIGGER ephemeral_child_lifecycle_delete_guard BEFORE DELETE ON ephemeral_child_lifecycles BEGIN SELECT RAISE(ABORT,'ephemeral child lifecycle is immutable'); END`},
}

func ephemeralChildLifecycleFullSchema() string {
	statements := []string{ephemeralChildLifecycleTable}
	for _, object := range ephemeralChildLifecycleObjects {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

func verifyEphemeralChildLifecycleSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, ephemeralChildLifecycleV61(ephemeralChildLifecycleTable)); err != nil {
		return err
	}
	for _, object := range ephemeralChildLifecycleObjects {
		if err := checkObject(ctx, tx, "trigger", object.name, ephemeralChildLifecycleV61(object.sql)); err != nil {
			return err
		}
	}
	return nil
}
