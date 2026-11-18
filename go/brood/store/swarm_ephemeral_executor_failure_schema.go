package store

import (
	"context"
	"database/sql"
	"strings"
)

const ephemeralChildExecutorFailureTable = `CREATE TABLE ephemeral_child_executor_failures (
 intent_id TEXT PRIMARY KEY NOT NULL REFERENCES ephemeral_child_allocations(intent_id) ON DELETE RESTRICT,
 child_id TEXT NOT NULL UNIQUE REFERENCES swarm_child_tasks(id) ON DELETE RESTRICT,
 reason TEXT NOT NULL CHECK(length(CAST(reason AS BLOB)) BETWEEN 1 AND 1024 AND length(trim(reason))>0),
 failed_at TEXT NOT NULL CHECK(failed_at<>'')
)`

var ephemeralChildExecutorFailureObjects = []struct{ name, sql string }{
	{"ephemeral_executor_failure_insert_guard", `CREATE TRIGGER ephemeral_executor_failure_insert_guard BEFORE INSERT ON ephemeral_child_executor_failures WHEN
 EXISTS(SELECT 1 FROM ephemeral_child_executor_failures WHERE intent_id=NEW.intent_id OR child_id=NEW.child_id)
 OR NOT EXISTS(SELECT 1 FROM ephemeral_child_intents e JOIN ephemeral_child_allocations a ON a.intent_id=e.id
 JOIN ephemeral_child_attestations p ON p.intent_id=e.id AND p.state='attested' AND p.actor_uid=a.actor_uid AND p.actor_incarnation=a.actor_incarnation AND p.origin=e.origin AND p.source_commit=e.source_commit AND p.attested_origin=e.origin AND p.attested_commit=e.source_commit
 JOIN swarm_child_tasks c ON c.swarm_id=e.swarm_id AND c.parent_sequence=e.parent_sequence AND c.request_id=e.request_id AND c.payload=e.payload AND c.actor_uid=a.actor_uid AND c.actor_incarnation=a.actor_incarnation
 JOIN swarm_child_joins j ON j.swarm_id=e.swarm_id AND j.parent_sequence=e.parent_sequence AND j.state='open' AND j.pass=e.pass AND j.project_id=e.project_id AND j.atespace_uid=e.atespace_uid AND j.parent_uid=e.parent_uid AND j.parent_incarnation=e.parent_incarnation AND j.parent_request_id=e.parent_request_id
 WHERE e.id=NEW.intent_id AND c.id=NEW.child_id AND c.state IN ('pending','running') AND c.role='' AND c.delivery=''
 AND NOT EXISTS(SELECT 1 FROM ephemeral_child_failures WHERE intent_id=e.id)
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions i WHERE i.swarm_id=e.swarm_id AND i.sequence=e.parent_sequence))
 BEGIN SELECT RAISE(ABORT,'registered executor failure scope conflicts'); END`},
	{"ephemeral_executor_failure_update_guard", `CREATE TRIGGER ephemeral_executor_failure_update_guard BEFORE UPDATE ON ephemeral_child_executor_failures BEGIN SELECT RAISE(ABORT,'registered executor failure is immutable'); END`},
	{"ephemeral_executor_failure_delete_guard", `CREATE TRIGGER ephemeral_executor_failure_delete_guard BEFORE DELETE ON ephemeral_child_executor_failures BEGIN SELECT RAISE(ABORT,'registered executor failure is immutable'); END`},
	{"ephemeral_executor_failed_task_guard", `CREATE TRIGGER ephemeral_executor_failed_task_guard BEFORE UPDATE ON swarm_child_tasks WHEN EXISTS(SELECT 1 FROM ephemeral_child_executor_failures WHERE child_id=OLD.id) BEGIN SELECT RAISE(ABORT,'server-failed child cannot change actor report or transport'); END`},
}

func ephemeralChildExecutorFailureFullSchema() string {
	statements := []string{ephemeralChildExecutorFailureTable}
	for _, o := range ephemeralChildExecutorFailureObjects {
		statements = append(statements, o.sql)
	}
	return strings.Join(statements, ";")
}

func ephemeralChildExecutorFailureMigration() string {
	statements := []string{ephemeralChildExecutorFailureFullSchema()}
	for _, o := range swarmChildObjects {
		switch o.name {
		case "swarm_steps_child_close", "swarm_steps_child_insert", "swarms_child_close":
			statements = append(statements, "DROP TRIGGER "+o.name, o.sql)
		}
	}
	statements = append(statements, "DROP TRIGGER ephemeral_child_result_seal_insert_guard", ephemeralChildResultSealV57(ephemeralChildResultSealInsertV55()))
	return strings.Join(statements, ";")
}

func ephemeralChildResultSealV57(sql string) string {
	return strings.ReplaceAll(sql, `t.state IN ('pending','running')`, `t.state IN ('pending','running') AND NOT EXISTS(SELECT 1 FROM ephemeral_child_executor_failures f WHERE f.child_id=t.id)`)
}

func verifyEphemeralChildExecutorFailureSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, ephemeralChildExecutorFailureTable); err != nil {
		return err
	}
	for _, o := range ephemeralChildExecutorFailureObjects {
		if err := checkObject(ctx, tx, "trigger", o.name, o.sql); err != nil {
			return err
		}
	}
	return nil
}
