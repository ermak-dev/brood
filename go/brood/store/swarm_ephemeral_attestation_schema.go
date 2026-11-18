package store

import (
	"context"
	"database/sql"
	"strings"
)

// Намерение и allocation неизменны и держат parent/job/pass/create linkage.
// Корневой lease хранится только здесь, не в DTO; завершённые строки не удаляются.
const ephemeralChildAttestationTable = `CREATE TABLE ephemeral_child_attestations (
 intent_id TEXT PRIMARY KEY NOT NULL REFERENCES ephemeral_child_allocations(intent_id) ON DELETE RESTRICT,
 attempt_id TEXT NOT NULL CHECK(length(attempt_id) BETWEEN 1 AND 128 AND attempt_id NOT GLOB '*[^a-zA-Z0-9_-]*'),
 actor_uid TEXT NOT NULL UNIQUE CHECK(actor_uid<>''),
 actor_incarnation INTEGER NOT NULL CHECK(actor_incarnation=1),
 origin TEXT NOT NULL CHECK(origin<>''),
 source_commit TEXT NOT NULL CHECK(source_commit<>''),
 lease TEXT NOT NULL CHECK(json_valid(lease)),
 state TEXT NOT NULL CHECK(state IN ('pending','unconfirmed','attested')),
 attested_origin TEXT NOT NULL,
 attested_commit TEXT NOT NULL,
 created_at TEXT NOT NULL CHECK(created_at<>''),
 updated_at TEXT NOT NULL CHECK(updated_at<>''),
 CHECK((state='attested' AND attested_origin=origin AND attested_commit=source_commit) OR (state IN ('pending','unconfirmed') AND attested_origin='' AND attested_commit=''))
)`

var ephemeralChildAttestationObjects = []struct{ name, sql string }{
	{"ephemeral_child_attestation_insert_guard", `CREATE TRIGGER ephemeral_child_attestation_insert_guard BEFORE INSERT ON ephemeral_child_attestations WHEN NEW.state!='pending' OR NOT EXISTS(SELECT 1 FROM ephemeral_child_allocations a JOIN ephemeral_child_intents e ON e.id=a.intent_id JOIN coordination_claims c ON c.project_id=e.project_id AND c.kind='workspace' AND c.slot=a.actor_uid AND c.payload=NEW.lease WHERE a.intent_id=NEW.intent_id AND a.actor_uid=NEW.actor_uid AND a.actor_incarnation=NEW.actor_incarnation AND e.origin=NEW.origin AND e.source_commit=NEW.source_commit AND json_extract(NEW.lease,'$.UID')=a.actor_uid AND json_extract(NEW.lease,'$.Generation')=a.actor_incarnation AND json_extract(NEW.lease,'$.SwarmID')=e.swarm_id AND json_extract(NEW.lease,'$.Operation')='workspace_attest' AND json_extract(NEW.lease,'$.State')='inflight' AND json_extract(NEW.lease,'$.OperationID')='ephemeral-attest:'||e.id||':'||NEW.attempt_id AND json_extract(NEW.lease,'$.Caller')='ephemeral:'||e.id) BEGIN SELECT RAISE(ABORT,'ephemeral child attestation scope conflicts'); END`},
	{"ephemeral_child_attestation_update_guard", `CREATE TRIGGER ephemeral_child_attestation_update_guard BEFORE UPDATE ON ephemeral_child_attestations WHEN NEW.intent_id!=OLD.intent_id OR NEW.attempt_id!=OLD.attempt_id OR NEW.actor_uid!=OLD.actor_uid OR NEW.actor_incarnation!=OLD.actor_incarnation OR NEW.origin!=OLD.origin OR NEW.source_commit!=OLD.source_commit OR NEW.lease!=OLD.lease OR NEW.created_at!=OLD.created_at OR NOT ((OLD.state='pending' AND NEW.state IN ('unconfirmed','attested')) OR (OLD.state='unconfirmed' AND NEW.state='attested')) BEGIN SELECT RAISE(ABORT,'ephemeral child attestation history is immutable'); END`},
	{"ephemeral_child_attestation_delete_guard", `CREATE TRIGGER ephemeral_child_attestation_delete_guard BEFORE DELETE ON ephemeral_child_attestations BEGIN SELECT RAISE(ABORT,'ephemeral child attestation history is immutable'); END`},
	{"ephemeral_child_attestation_replace_guard", `CREATE TRIGGER ephemeral_child_attestation_replace_guard BEFORE INSERT ON ephemeral_child_attestations WHEN EXISTS(SELECT 1 FROM ephemeral_child_attestations WHERE intent_id=NEW.intent_id OR actor_uid=NEW.actor_uid) BEGIN SELECT RAISE(ABORT,'ephemeral child attestation history is immutable'); END`},
}

func ephemeralChildAttestationFullSchema() string {
	statements := []string{ephemeralChildAttestationTable}
	for _, object := range ephemeralChildAttestationObjects {
		statements = append(statements, object.sql)
	}
	return strings.Join(statements, ";")
}

func verifyEphemeralChildAttestationSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, ephemeralChildAttestationTable); err != nil {
		return err
	}
	for _, object := range ephemeralChildAttestationObjects {
		if err := checkObject(ctx, tx, "trigger", object.name, object.sql); err != nil {
			return err
		}
	}
	return nil
}
