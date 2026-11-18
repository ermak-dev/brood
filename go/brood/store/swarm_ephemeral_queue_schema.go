package store

import (
	"context"
	"database/sql"
	"strings"
)

const ephemeralChildIntentTable = `CREATE TABLE ephemeral_child_intents (
 id TEXT PRIMARY KEY NOT NULL CHECK(id<>''),
 swarm_id TEXT NOT NULL CHECK(swarm_id<>''),
 parent_sequence INTEGER NOT NULL CHECK(parent_sequence>0),
 pass INTEGER NOT NULL CHECK(pass>0 AND pass<=parent_sequence),
 project_id TEXT NOT NULL CHECK(project_id<>''),
 atespace_uid TEXT NOT NULL CHECK(atespace_uid<>''),
 parent_uid TEXT NOT NULL CHECK(parent_uid<>''),
 parent_incarnation INTEGER NOT NULL CHECK(parent_incarnation>0),
 parent_request_id TEXT NOT NULL CHECK(parent_request_id<>''),
 request_id TEXT NOT NULL CHECK(request_id<>''),
 payload TEXT NOT NULL CHECK(length(CAST(payload AS BLOB)) BETWEEN 1 AND 4096),
 pool_uid TEXT NOT NULL CHECK(pool_uid<>''),
 source_fingerprint TEXT NOT NULL CHECK(source_fingerprint<>''),
 template TEXT NOT NULL CHECK(template<>''),
 source_commit TEXT NOT NULL CHECK(source_commit<>''),
 origin TEXT NOT NULL CHECK(origin<>''),
 created_at TEXT NOT NULL CHECK(created_at<>''),
 UNIQUE(swarm_id,parent_sequence,request_id)
)`

// Замороженный DDL версии 47: миграция 46→47 не использует новые ограды.
var ephemeralChildIntentObjectsV47 = []struct{ name, kind, sql string }{
	{"ephemeral_child_intent_update_guard", "trigger", `CREATE TRIGGER ephemeral_child_intent_update_guard BEFORE UPDATE ON ephemeral_child_intents BEGIN SELECT RAISE(ABORT,'ephemeral child intent history is immutable'); END`},
	{"ephemeral_child_intent_delete_guard", "trigger", `CREATE TRIGGER ephemeral_child_intent_delete_guard BEFORE DELETE ON ephemeral_child_intents BEGIN SELECT RAISE(ABORT,'ephemeral child intent history is immutable'); END`},
	{"ephemeral_child_intent_replace_guard", "trigger", `CREATE TRIGGER ephemeral_child_intent_replace_guard BEFORE INSERT ON ephemeral_child_intents WHEN EXISTS(SELECT 1 FROM ephemeral_child_intents WHERE id=NEW.id OR (swarm_id=NEW.swarm_id AND parent_sequence=NEW.parent_sequence AND request_id=NEW.request_id)) BEGIN SELECT RAISE(ABORT,'ephemeral child intent history is immutable'); END`},
	{"ephemeral_child_intent_insert_guard", "trigger", `CREATE TRIGGER ephemeral_child_intent_insert_guard BEFORE INSERT ON ephemeral_child_intents WHEN NOT EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=NEW.swarm_id AND j.parent_sequence=NEW.parent_sequence AND j.pass=NEW.pass AND j.project_id=NEW.project_id AND j.atespace_uid=NEW.atespace_uid AND j.parent_uid=NEW.parent_uid AND j.parent_incarnation=NEW.parent_incarnation AND j.parent_request_id=NEW.parent_request_id AND j.state='open') OR EXISTS(SELECT 1 FROM swarm_child_tasks c WHERE c.swarm_id=NEW.swarm_id AND c.parent_sequence=NEW.parent_sequence AND c.request_id=NEW.request_id) OR (SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=NEW.swarm_id AND c.parent_sequence=NEW.parent_sequence)+(SELECT count(*) FROM ephemeral_child_intents e WHERE e.swarm_id=NEW.swarm_id AND e.parent_sequence=NEW.parent_sequence)>=(SELECT total FROM swarm_child_joins j WHERE j.swarm_id=NEW.swarm_id AND j.parent_sequence=NEW.parent_sequence) BEGIN SELECT RAISE(ABORT,'ephemeral child slot conflicts'); END`},
	{"ephemeral_child_task_insert_guard", "trigger", `CREATE TRIGGER ephemeral_child_task_insert_guard BEFORE INSERT ON swarm_child_tasks WHEN EXISTS(SELECT 1 FROM ephemeral_child_intents e WHERE e.swarm_id=NEW.swarm_id AND e.parent_sequence=NEW.parent_sequence) AND (EXISTS(SELECT 1 FROM ephemeral_child_intents e WHERE e.swarm_id=NEW.swarm_id AND e.parent_sequence=NEW.parent_sequence AND e.request_id=NEW.request_id) OR (SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=NEW.swarm_id AND c.parent_sequence=NEW.parent_sequence)+(SELECT count(*) FROM ephemeral_child_intents e WHERE e.swarm_id=NEW.swarm_id AND e.parent_sequence=NEW.parent_sequence)>=(SELECT total FROM swarm_child_joins j WHERE j.swarm_id=NEW.swarm_id AND j.parent_sequence=NEW.parent_sequence)) BEGIN SELECT RAISE(ABORT,'ephemeral child slot conflicts'); END`},
}

func ephemeralChildIntentSchemaV47() string {
	statements := []string{ephemeralChildIntentTable}
	for _, object := range ephemeralChildIntentObjectsV47 {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

func ephemeralChildIntentFullSchema() string {
	statements := []string{ephemeralChildIntentTable}
	for _, object := range ephemeralChildIntentObjects {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

func verifyEphemeralChildIntentSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, ephemeralChildIntentTable); err != nil {
		return err
	}
	for _, object := range ephemeralChildIntentObjects {
		if err := checkObject(ctx, tx, object.kind, object.name, object.sql); err != nil {
			return err
		}
	}
	return nil
}
