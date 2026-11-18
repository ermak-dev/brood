package store

import (
	"context"
	"database/sql"
	"strings"
)

const pipelinePolicyConfirmationSchema = `CREATE TABLE pipeline_policy_confirmations (
 candidate_id TEXT NOT NULL PRIMARY KEY,
 swarm_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision=2),
 user_id TEXT NOT NULL CHECK (length(user_id)>0),
 answer TEXT NOT NULL CHECK (answer IN ('yes','no')),
 answered_at TEXT NOT NULL CHECK (length(answered_at)>0),
 FOREIGN KEY (candidate_id) REFERENCES pipeline_policy_candidates(id)
);`

var pipelinePolicyConfirmationTriggers = []struct{ name, sql string }{
	{"pipeline_policy_confirmations_insert", `CREATE TRIGGER pipeline_policy_confirmations_insert BEFORE INSERT ON pipeline_policy_confirmations WHEN NOT EXISTS (
 SELECT 1 FROM pipeline_policy_candidates c JOIN swarms sw ON sw.id=c.swarm_id JOIN swarm_steps st ON st.swarm_id=c.swarm_id AND st.sequence=c.sequence JOIN users u ON u.id=NEW.user_id
 WHERE c.id=NEW.candidate_id AND c.swarm_id=NEW.swarm_id AND sw.state='active' AND sw.execution IN ('running','waiting')
 AND c.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=c.swarm_id)
 AND json_extract(c.preview,'$.Decision.State')='waiting_human' AND json_array_length(c.preview,'$.Decision.Human')>0
 AND u.disabled=0 AND (u.is_admin=1 OR (EXISTS(SELECT 1 FROM project_members WHERE project_id=sw.project_id AND user_id=u.id) AND EXISTS(SELECT 1 FROM swarm_members WHERE swarm_id=sw.id AND user_id=u.id AND role IN ('owner','editor'))))
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions WHERE swarm_id=c.swarm_id AND sequence=c.sequence)
 AND ((c.boundary='entry' AND st.state IN ('pending','waiting') AND st.actor_uid IS NULL AND st.request_id IS NULL)
 OR (c.boundary='result' AND st.state IN ('inflight','delivering','unconfirmed') AND EXISTS(SELECT 1 FROM pipeline_result_observations WHERE id=c.evidence_id AND swarm_id=c.swarm_id AND sequence=c.sequence AND state='observed')))
 ) BEGIN SELECT RAISE(ABORT, 'policy confirmation requires current manager and candidate'); END`},
	{"pipeline_policy_confirmations_no_update", `CREATE TRIGGER pipeline_policy_confirmations_no_update BEFORE UPDATE ON pipeline_policy_confirmations BEGIN SELECT RAISE(ABORT, 'policy confirmation history is immutable'); END`},
	{"pipeline_policy_confirmations_no_delete", `CREATE TRIGGER pipeline_policy_confirmations_no_delete BEFORE DELETE ON pipeline_policy_confirmations BEGIN SELECT RAISE(ABORT, 'policy confirmation history is immutable'); END`},
	{"pipeline_policy_confirmations_no_replace", `CREATE TRIGGER pipeline_policy_confirmations_no_replace BEFORE INSERT ON pipeline_policy_confirmations WHEN EXISTS(SELECT 1 FROM pipeline_policy_confirmations WHERE candidate_id=NEW.candidate_id) BEGIN SELECT RAISE(ABORT, 'policy confirmation history is immutable'); END`},
}

func pipelinePolicyConfirmationFullSchema() string {
	statements := []string{pipelinePolicyConfirmationSchema}
	for _, trigger := range pipelinePolicyConfirmationTriggers {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}

func verifyPipelinePolicyConfirmationSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, pipelinePolicyConfirmationSchema); err != nil {
		return err
	}
	for _, trigger := range pipelinePolicyConfirmationTriggers {
		if err := checkObject(ctx, tx, "trigger", trigger.name, trigger.sql); err != nil {
			return err
		}
	}
	return nil
}
