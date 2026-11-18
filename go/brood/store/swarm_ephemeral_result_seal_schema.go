package store

import (
	"context"
	"database/sql"
	"strings"
)

const ephemeralChildResultSealTable = `CREATE TABLE ephemeral_child_result_seals (
 intent_id TEXT PRIMARY KEY NOT NULL REFERENCES ephemeral_child_allocations(intent_id) ON DELETE RESTRICT,
 child_id TEXT NOT NULL UNIQUE REFERENCES swarm_child_tasks(id) ON DELETE RESTRICT,
 child_revision INTEGER NOT NULL CHECK(child_revision>=3),
 outcome TEXT NOT NULL CHECK(outcome IN ('success','fail')),
 report_hash TEXT NOT NULL CHECK(length(report_hash)=64 AND report_hash NOT GLOB '*[^0-9a-f]*'),
 parent_outcome TEXT NOT NULL CHECK(parent_outcome IN ('success','fail')),
 parent_report_hash TEXT NOT NULL CHECK(length(parent_report_hash)=64 AND parent_report_hash NOT GLOB '*[^0-9a-f]*'),
 created_at TEXT NOT NULL CHECK(created_at<>'')
)`

var ephemeralChildResultSealObjects = []struct{ name, sql string }{
	{"ephemeral_child_result_seal_insert_guard", `CREATE TRIGGER ephemeral_child_result_seal_insert_guard BEFORE INSERT ON ephemeral_child_result_seals WHEN EXISTS(SELECT 1 FROM ephemeral_child_result_seals WHERE intent_id=NEW.intent_id OR child_id=NEW.child_id) OR NOT EXISTS(
 SELECT 1 FROM ephemeral_child_intents e
 JOIN ephemeral_child_allocations a ON a.intent_id=e.id
 JOIN ephemeral_child_attestations p ON p.intent_id=e.id AND p.state='attested' AND p.actor_uid=a.actor_uid AND p.actor_incarnation=a.actor_incarnation AND p.attested_origin=e.origin AND p.attested_commit=e.source_commit
 JOIN swarm_child_tasks c ON c.swarm_id=e.swarm_id AND c.parent_sequence=e.parent_sequence AND c.request_id=e.request_id AND c.payload=e.payload AND c.actor_uid=a.actor_uid AND c.actor_incarnation=a.actor_incarnation
 JOIN swarm_child_joins j ON j.swarm_id=e.swarm_id AND j.parent_sequence=e.parent_sequence AND j.state='open'
 JOIN swarm_steps v ON v.swarm_id=e.swarm_id AND v.sequence=e.parent_sequence
 WHERE e.id=NEW.intent_id AND c.id=NEW.child_id AND c.state='completed' AND c.role='' AND c.delivery='' AND c.revision=NEW.child_revision AND c.outcome=NEW.outcome AND length(c.report)>0
 AND v.state='completed' AND v.outcome=NEW.parent_outcome AND v.actor_uid=e.parent_uid AND v.actor_incarnation=e.parent_incarnation AND v.request_id=e.parent_request_id AND v.attested_commit=e.source_commit
 AND (SELECT count(*) FROM swarm_child_tasks t WHERE t.swarm_id=e.swarm_id AND t.parent_sequence=e.parent_sequence)>=j.total
 AND NOT EXISTS(SELECT 1 FROM swarm_child_tasks t WHERE t.swarm_id=e.swarm_id AND t.parent_sequence=e.parent_sequence AND t.state IN ('pending','running'))
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions i WHERE i.swarm_id=e.swarm_id AND i.sequence=e.parent_sequence)
 ) BEGIN SELECT RAISE(ABORT,'ephemeral child result evidence conflicts'); END`},
	{"ephemeral_child_result_seal_update_guard", `CREATE TRIGGER ephemeral_child_result_seal_update_guard BEFORE UPDATE ON ephemeral_child_result_seals BEGIN SELECT RAISE(ABORT,'ephemeral child result evidence is immutable'); END`},
	{"ephemeral_child_result_seal_delete_guard", `CREATE TRIGGER ephemeral_child_result_seal_delete_guard BEFORE DELETE ON ephemeral_child_result_seals BEGIN SELECT RAISE(ABORT,'ephemeral child result evidence is immutable'); END`},
}

func ephemeralChildResultSealFullSchema() string {
	statements := []string{ephemeralChildResultSealTable}
	for _, object := range ephemeralChildResultSealObjects {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

func verifyEphemeralChildResultSealSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, ephemeralChildResultSealTable); err != nil {
		return err
	}
	for _, object := range ephemeralChildResultSealObjects {
		if err := checkObject(ctx, tx, "trigger", object.name, ephemeralChildResultSealV57(ephemeralChildResultSealV55(object.sql))); err != nil {
			return err
		}
	}
	return nil
}
