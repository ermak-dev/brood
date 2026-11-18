package store

import (
	"context"
	"database/sql"
	"strings"
)

const pipelineResultSchema = `CREATE TABLE pipeline_result_observations (
 id TEXT PRIMARY KEY NOT NULL CHECK (length(id) BETWEEN 1 AND 128),
 swarm_id TEXT NOT NULL,
 sequence INTEGER NOT NULL CHECK (sequence > 0),
 request TEXT NOT NULL CHECK (length(request) BETWEEN 2 AND 8192 AND json_valid(request) AND json_type(request)='object'),
 lease TEXT NOT NULL CHECK (length(lease) BETWEEN 2 AND 8192 AND json_valid(lease) AND json_type(lease)='object'),
 state TEXT NOT NULL CHECK (state IN ('inflight','observed','refused','unconfirmed')),
 detail TEXT NOT NULL CHECK (length(CAST(detail AS BLOB)) <= 1024),
 revision INTEGER NOT NULL CHECK (revision > 0),
 UNIQUE (swarm_id,sequence),
 FOREIGN KEY (swarm_id,sequence) REFERENCES pipeline_completion_intents(swarm_id,sequence)
);
CREATE TABLE pipeline_result_evidence (
 observation_id TEXT PRIMARY KEY NOT NULL REFERENCES pipeline_result_observations(id),
 attestation TEXT NOT NULL CHECK (length(CAST(attestation AS BLOB)) BETWEEN 2 AND 98304 AND json_valid(attestation) AND json_type(attestation)='object')
);`

var pipelineResultTriggers = []struct{ name, sql string }{
	{"pipeline_result_observations_insert", `CREATE TRIGGER pipeline_result_observations_insert BEFORE INSERT ON pipeline_result_observations WHEN NEW.state!='inflight' OR NEW.revision!=1 OR NEW.detail!='' OR NOT EXISTS (
 SELECT 1 FROM pipeline_completion_intents i JOIN coordination_claims c ON c.kind='workspace' AND c.request_id=NEW.id AND c.slot=i.actor_uid
 WHERE i.swarm_id=NEW.swarm_id AND i.sequence=NEW.sequence AND c.payload=NEW.lease
 AND json_extract(NEW.request,'$.actor_uid')=i.actor_uid AND json_extract(NEW.request,'$.generation')=i.actor_incarnation
 AND json_extract(NEW.request,'$.swarm')=i.swarm_id AND json_extract(NEW.request,'$.step')=i.request_id
 AND json_extract(NEW.request,'$.origin')=i.origin AND json_extract(NEW.request,'$.commit')=i.base_commit
 AND json_extract(NEW.request,'$.pass')=i.pass_sequence AND json_extract(NEW.request,'$.sequence')=i.sequence
 AND json_extract(NEW.lease,'$.Operation')='workspace_result' AND json_extract(NEW.lease,'$.State')='inflight'
 ) BEGIN SELECT RAISE(ABORT, 'result observation requires the exact completion lease'); END`},
	{"pipeline_result_observations_fence", `CREATE TRIGGER pipeline_result_observations_fence BEFORE UPDATE ON pipeline_result_observations WHEN NEW.id IS NOT OLD.id OR NEW.swarm_id IS NOT OLD.swarm_id OR NEW.sequence IS NOT OLD.sequence OR NEW.request IS NOT OLD.request OR NEW.lease IS NOT OLD.lease OR OLD.state!='inflight' OR NEW.state NOT IN ('observed','refused','unconfirmed') OR NEW.revision!=OLD.revision+1 OR ((NEW.state='observed') != EXISTS(SELECT 1 FROM pipeline_result_evidence WHERE observation_id=OLD.id)) BEGIN SELECT RAISE(ABORT, 'result observation transition is invalid'); END`},
	{"pipeline_result_observations_no_delete", `CREATE TRIGGER pipeline_result_observations_no_delete BEFORE DELETE ON pipeline_result_observations BEGIN SELECT RAISE(ABORT, 'result observation history is immutable'); END`},
	{"pipeline_result_observations_no_replace", `CREATE TRIGGER pipeline_result_observations_no_replace BEFORE INSERT ON pipeline_result_observations WHEN EXISTS (SELECT 1 FROM pipeline_result_observations WHERE id=NEW.id OR (swarm_id=NEW.swarm_id AND sequence=NEW.sequence)) BEGIN SELECT RAISE(ABORT, 'result observation history is immutable'); END`},
	{"pipeline_result_evidence_insert", `CREATE TRIGGER pipeline_result_evidence_insert BEFORE INSERT ON pipeline_result_evidence WHEN NOT EXISTS(SELECT 1 FROM pipeline_result_observations WHERE id=NEW.observation_id AND state='inflight') BEGIN SELECT RAISE(ABORT, 'result evidence requires an inflight observation'); END`},
	{"pipeline_result_evidence_no_update", `CREATE TRIGGER pipeline_result_evidence_no_update BEFORE UPDATE ON pipeline_result_evidence BEGIN SELECT RAISE(ABORT, 'pipeline result evidence is immutable'); END`},
	{"pipeline_result_evidence_no_delete", `CREATE TRIGGER pipeline_result_evidence_no_delete BEFORE DELETE ON pipeline_result_evidence BEGIN SELECT RAISE(ABORT, 'pipeline result evidence is immutable'); END`},
	{"pipeline_result_evidence_no_replace", `CREATE TRIGGER pipeline_result_evidence_no_replace BEFORE INSERT ON pipeline_result_evidence WHEN EXISTS(SELECT 1 FROM pipeline_result_evidence WHERE observation_id=NEW.observation_id) BEGIN SELECT RAISE(ABORT, 'pipeline result evidence is immutable'); END`},
}

func pipelineResultFullSchema() string {
	statements := []string{pipelineResultSchema}
	for _, trigger := range pipelineResultTriggers {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}
func verifyPipelineResultSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, pipelineResultSchema); err != nil {
		return err
	}
	for _, trigger := range pipelineResultTriggers {
		if err := checkObject(ctx, tx, "trigger", trigger.name, trigger.sql); err != nil {
			return err
		}
	}
	return nil
}
