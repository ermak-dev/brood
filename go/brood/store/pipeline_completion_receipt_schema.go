package store

import (
	"context"
	"database/sql"
	"strings"
)

const pipelineCompletionReceiptSchema = `CREATE TABLE pipeline_completion_receipts (
 swarm_id TEXT NOT NULL,
 sequence INTEGER NOT NULL CHECK (sequence > 0),
 version INTEGER NOT NULL CHECK (version=1),
 nonce TEXT NOT NULL UNIQUE CHECK (length(nonce)=43 AND nonce NOT GLOB '*[^a-zA-Z0-9_-]*'),
 created_at TEXT NOT NULL CHECK (length(created_at) > 0),
 PRIMARY KEY (swarm_id,sequence),
 FOREIGN KEY (swarm_id,sequence) REFERENCES pipeline_completion_intents(swarm_id,sequence)
);
CREATE TABLE pipeline_completion_acknowledgements (
 swarm_id TEXT NOT NULL,
 sequence INTEGER NOT NULL CHECK (sequence > 0),
 acknowledged_at TEXT NOT NULL CHECK (length(acknowledged_at) > 0),
 PRIMARY KEY (swarm_id,sequence),
 FOREIGN KEY (swarm_id,sequence) REFERENCES pipeline_completion_receipts(swarm_id,sequence)
);`

var pipelineCompletionReceiptTriggers = []struct{ name, sql string }{
	{"pipeline_completion_receipts_no_update", `CREATE TRIGGER pipeline_completion_receipts_no_update BEFORE UPDATE ON pipeline_completion_receipts BEGIN SELECT RAISE(ABORT, 'completion receipt history is immutable'); END`},
	{"pipeline_completion_receipts_no_delete", `CREATE TRIGGER pipeline_completion_receipts_no_delete BEFORE DELETE ON pipeline_completion_receipts BEGIN SELECT RAISE(ABORT, 'completion receipt history is immutable'); END`},
	{"pipeline_completion_receipts_no_replace", `CREATE TRIGGER pipeline_completion_receipts_no_replace BEFORE INSERT ON pipeline_completion_receipts WHEN EXISTS(SELECT 1 FROM pipeline_completion_receipts WHERE swarm_id=NEW.swarm_id AND sequence=NEW.sequence) BEGIN SELECT RAISE(ABORT, 'completion receipt history is immutable'); END`},
	{"pipeline_completion_acknowledgements_insert", `CREATE TRIGGER pipeline_completion_acknowledgements_insert BEFORE INSERT ON pipeline_completion_acknowledgements WHEN NOT EXISTS (
 SELECT 1 FROM pipeline_completion_receipts r JOIN pipeline_completion_intents i ON i.swarm_id=r.swarm_id AND i.sequence=r.sequence JOIN swarm_steps st ON st.swarm_id=i.swarm_id AND st.sequence=i.sequence JOIN swarms sw ON sw.id=st.swarm_id
 WHERE r.swarm_id=NEW.swarm_id AND r.sequence=NEW.sequence AND st.actor_uid=i.actor_uid AND st.actor_incarnation=i.actor_incarnation AND st.request_id=i.request_id AND st.attested_commit=i.base_commit
 AND st.state IN ('inflight','delivering','unconfirmed') AND sw.state='active' AND sw.execution IN ('running','waiting')
 AND st.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=NEW.swarm_id)
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions WHERE swarm_id=NEW.swarm_id AND sequence=NEW.sequence)
 ) BEGIN SELECT RAISE(ABORT, 'completion acknowledgement requires the current attempt'); END`},
	{"pipeline_completion_acknowledgements_no_update", `CREATE TRIGGER pipeline_completion_acknowledgements_no_update BEFORE UPDATE ON pipeline_completion_acknowledgements BEGIN SELECT RAISE(ABORT, 'completion receipt history is immutable'); END`},
	{"pipeline_completion_acknowledgements_no_delete", `CREATE TRIGGER pipeline_completion_acknowledgements_no_delete BEFORE DELETE ON pipeline_completion_acknowledgements BEGIN SELECT RAISE(ABORT, 'completion receipt history is immutable'); END`},
	{"pipeline_completion_acknowledgements_no_replace", `CREATE TRIGGER pipeline_completion_acknowledgements_no_replace BEFORE INSERT ON pipeline_completion_acknowledgements WHEN EXISTS(SELECT 1 FROM pipeline_completion_acknowledgements WHERE swarm_id=NEW.swarm_id AND sequence=NEW.sequence) BEGIN SELECT RAISE(ABORT, 'completion receipt history is immutable'); END`},
	{"pipeline_result_observations_receipt", `CREATE TRIGGER pipeline_result_observations_receipt BEFORE INSERT ON pipeline_result_observations WHEN NOT EXISTS(SELECT 1 FROM pipeline_completion_acknowledgements WHERE swarm_id=NEW.swarm_id AND sequence=NEW.sequence) BEGIN SELECT RAISE(ABORT, 'result observation requires a client completion receipt'); END`},
}

func pipelineCompletionReceiptFullSchema() string {
	statements := []string{pipelineCompletionReceiptSchema}
	for _, trigger := range pipelineCompletionReceiptTriggers {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}
func verifyPipelineCompletionReceiptSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, pipelineCompletionReceiptSchema); err != nil {
		return err
	}
	for _, trigger := range pipelineCompletionReceiptTriggers {
		if err := checkObject(ctx, tx, "trigger", trigger.name, trigger.sql); err != nil {
			return err
		}
	}
	return nil
}
