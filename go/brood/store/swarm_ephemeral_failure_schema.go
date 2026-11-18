package store

import (
	"context"
	"database/sql"
	"strings"
)

const ephemeralChildFailureTable = `CREATE TABLE ephemeral_child_failures (
 intent_id TEXT PRIMARY KEY NOT NULL REFERENCES ephemeral_child_intents(id) ON DELETE RESTRICT,
 reason TEXT NOT NULL CHECK(length(CAST(reason AS BLOB)) BETWEEN 1 AND 1024 AND length(trim(reason))>0),
 failed_at TEXT NOT NULL CHECK(failed_at<>'')
)`

var ephemeralChildFailureObjects = []struct{ name, sql string }{
	{"ephemeral_child_failure_insert_guard", `CREATE TRIGGER ephemeral_child_failure_insert_guard BEFORE INSERT ON ephemeral_child_failures WHEN
 EXISTS(SELECT 1 FROM ephemeral_child_failures WHERE intent_id=NEW.intent_id)
 OR NOT EXISTS(SELECT 1 FROM ephemeral_child_intents e JOIN swarm_child_joins j ON j.swarm_id=e.swarm_id AND j.parent_sequence=e.parent_sequence
 WHERE e.id=NEW.intent_id AND j.state='open' AND j.pass=e.pass AND j.project_id=e.project_id AND j.atespace_uid=e.atespace_uid AND j.parent_uid=e.parent_uid AND j.parent_incarnation=e.parent_incarnation AND j.parent_request_id=e.parent_request_id
 AND NOT EXISTS(SELECT 1 FROM swarm_child_tasks c WHERE c.swarm_id=e.swarm_id AND c.parent_sequence=e.parent_sequence AND c.request_id=e.request_id))
 BEGIN SELECT RAISE(ABORT,'ephemeral child failure is immutable or already registered'); END`},
	{"ephemeral_child_failure_update_guard", `CREATE TRIGGER ephemeral_child_failure_update_guard BEFORE UPDATE ON ephemeral_child_failures BEGIN SELECT RAISE(ABORT,'ephemeral child failure is immutable'); END`},
	{"ephemeral_child_failure_delete_guard", `CREATE TRIGGER ephemeral_child_failure_delete_guard BEFORE DELETE ON ephemeral_child_failures BEGIN SELECT RAISE(ABORT,'ephemeral child failure is immutable'); END`},
	{"ephemeral_failed_child_insert_guard", `CREATE TRIGGER ephemeral_failed_child_insert_guard BEFORE INSERT ON swarm_child_tasks WHEN EXISTS(SELECT 1 FROM ephemeral_child_intents e JOIN ephemeral_child_failures f ON f.intent_id=e.id WHERE e.swarm_id=NEW.swarm_id AND e.parent_sequence=NEW.parent_sequence AND e.request_id=NEW.request_id) BEGIN SELECT RAISE(ABORT,'failed ephemeral intent cannot register a child'); END`},
	{"ephemeral_failed_allocation_guard", `CREATE TRIGGER ephemeral_failed_allocation_guard BEFORE INSERT ON ephemeral_child_allocations WHEN EXISTS(SELECT 1 FROM ephemeral_child_failures WHERE intent_id=NEW.intent_id) BEGIN SELECT RAISE(ABORT,'failed ephemeral intent cannot allocate'); END`},
	{"ephemeral_failed_attestation_guard", `CREATE TRIGGER ephemeral_failed_attestation_guard BEFORE INSERT ON ephemeral_child_attestations WHEN EXISTS(SELECT 1 FROM ephemeral_child_failures WHERE intent_id=NEW.intent_id) BEGIN SELECT RAISE(ABORT,'failed ephemeral intent cannot attest'); END`},
	{"ephemeral_failed_attestation_proof_guard", `CREATE TRIGGER ephemeral_failed_attestation_proof_guard BEFORE UPDATE ON ephemeral_child_attestations WHEN NEW.state='attested' AND EXISTS(SELECT 1 FROM ephemeral_child_failures WHERE intent_id=NEW.intent_id) BEGIN SELECT RAISE(ABORT,'failed ephemeral intent cannot accept proof'); END`},
	{"ephemeral_failed_lifecycle_guard", `CREATE TRIGGER ephemeral_failed_lifecycle_guard BEFORE INSERT ON ephemeral_child_lifecycles WHEN EXISTS(SELECT 1 FROM ephemeral_child_failures WHERE intent_id=NEW.intent_id) BEGIN SELECT RAISE(ABORT,'failed ephemeral intent cannot begin automatic lifecycle'); END`},
}

func ephemeralChildFailureFullSchema() string {
	statements := []string{ephemeralChildFailureTable}
	for _, o := range ephemeralChildFailureObjects {
		statements = append(statements, o.sql)
	}
	return strings.Join(statements, ";")
}

func ephemeralChildFailureMigration() string {
	statements := []string{ephemeralChildFailureFullSchema()}
	// Три SQL hold-триггера должны считать terminal intent без synthetic child.
	for _, o := range swarmChildObjects {
		switch o.name {
		case "swarm_steps_child_close", "swarm_steps_child_insert", "swarms_child_close":
			statements = append(statements, "DROP TRIGGER "+o.name, strings.ReplaceAll(o.sql, swarmChildUnfinished, swarmChildUnfinishedV55))
		}
	}
	statements = append(statements, "DROP TRIGGER ephemeral_child_result_seal_insert_guard", ephemeralChildResultSealInsertV55())
	return strings.Join(statements, ";")
}

// Меняем только aggregate membership: сам seal по-прежнему требует настоящие
// allocation, proof и completed child. Failure не становится независимым report.
func ephemeralChildResultSealV55(sql string) string {
	return strings.ReplaceAll(sql,
		`(SELECT count(*) FROM swarm_child_tasks t WHERE t.swarm_id=e.swarm_id AND t.parent_sequence=e.parent_sequence)>=j.total`,
		`((SELECT count(*) FROM swarm_child_tasks t WHERE t.swarm_id=e.swarm_id AND t.parent_sequence=e.parent_sequence)+(SELECT count(*) FROM ephemeral_child_failures f JOIN ephemeral_child_intents i ON i.id=f.intent_id WHERE i.swarm_id=e.swarm_id AND i.parent_sequence=e.parent_sequence))=j.total`)
}

func ephemeralChildResultSealInsertV55() string {
	return ephemeralChildResultSealV55(ephemeralChildResultSealObjects[0].sql)
}

func ephemeralChildResultSealFullSchemaV55() string {
	return ephemeralChildResultSealV55(ephemeralChildResultSealFullSchema())
}

func verifyEphemeralChildFailureSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, ephemeralChildFailureTable); err != nil {
		return err
	}
	for _, o := range ephemeralChildFailureObjects {
		if err := checkObject(ctx, tx, "trigger", o.name, ephemeralFailedLifecycleV58(o.sql)); err != nil {
			return err
		}
	}
	return nil
}
