package store

import (
	"context"
	"database/sql"
	"strings"
)

const pipelineCompletionIntentSchema = `CREATE TABLE pipeline_completion_intents (
 swarm_id TEXT NOT NULL CHECK (length(swarm_id) > 0),
 sequence INTEGER NOT NULL CHECK (sequence > 0),
 pass_sequence INTEGER NOT NULL CHECK (pass_sequence > 0 AND pass_sequence <= sequence),
 request_id TEXT NOT NULL CHECK (length(request_id) BETWEEN 1 AND 4096),
 actor_uid TEXT NOT NULL CHECK (length(actor_uid) BETWEEN 1 AND 4096),
 actor_incarnation INTEGER NOT NULL CHECK (actor_incarnation > 0),
 origin TEXT NOT NULL CHECK (length(origin) BETWEEN 1 AND 1024),
 base_commit TEXT NOT NULL CHECK (length(base_commit)=40 AND base_commit NOT GLOB '*[^0-9a-f]*'),
 outcome TEXT NOT NULL CHECK (outcome IN ('success','fail')),
 report TEXT NOT NULL CHECK (length(CAST(report AS BLOB)) <= 16384),
 created_at TEXT NOT NULL CHECK (length(created_at) > 0),
 PRIMARY KEY (swarm_id,sequence),
 UNIQUE (swarm_id,request_id),
 FOREIGN KEY (swarm_id,pass_sequence) REFERENCES swarm_pass_policies(swarm_id,pass_sequence)
);`

var pipelineCompletionIntentTriggers = []struct{ name, sql string }{
	{"pipeline_completion_intents_insert", `CREATE TRIGGER pipeline_completion_intents_insert BEFORE INSERT ON pipeline_completion_intents WHEN NOT EXISTS (
 SELECT 1 FROM swarm_steps st JOIN swarms sw ON sw.id=st.swarm_id JOIN pipeline_sources src ON src.swarm_id=sw.id JOIN swarm_steps first ON first.swarm_id=st.swarm_id AND first.sequence=NEW.pass_sequence
 WHERE st.swarm_id=NEW.swarm_id AND st.sequence=NEW.sequence AND st.request_id=NEW.request_id AND st.actor_uid=NEW.actor_uid AND st.actor_incarnation=NEW.actor_incarnation AND st.attested_commit=NEW.base_commit
 AND first.goal IS NOT NULL AND first.source_commit=NEW.base_commit AND src.origin=NEW.origin AND sw.state='active' AND sw.execution IN ('running','waiting') AND st.state IN ('inflight','delivering','unconfirmed')
 AND st.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=NEW.swarm_id)
 AND NEW.pass_sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=NEW.swarm_id AND sequence<=NEW.sequence AND goal IS NOT NULL)
 ) BEGIN SELECT RAISE(ABORT, 'pipeline completion intent requires the current verified attempt'); END`},
	{"pipeline_completion_intents_no_update", `CREATE TRIGGER pipeline_completion_intents_no_update BEFORE UPDATE ON pipeline_completion_intents BEGIN SELECT RAISE(ABORT, 'pipeline completion intent is immutable'); END`},
	{"pipeline_completion_intents_no_delete", `CREATE TRIGGER pipeline_completion_intents_no_delete BEFORE DELETE ON pipeline_completion_intents BEGIN SELECT RAISE(ABORT, 'pipeline completion intent is immutable'); END`},
	{"pipeline_completion_intents_no_replace", `CREATE TRIGGER pipeline_completion_intents_no_replace BEFORE INSERT ON pipeline_completion_intents WHEN EXISTS (SELECT 1 FROM pipeline_completion_intents WHERE swarm_id=NEW.swarm_id AND sequence=NEW.sequence) BEGIN SELECT RAISE(ABORT, 'pipeline completion intent is immutable'); END`},
}

func pipelineCompletionIntentFullSchema() string {
	statements := []string{pipelineCompletionIntentSchema}
	for _, trigger := range pipelineCompletionIntentTriggers {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}
func verifyPipelineCompletionIntentSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, pipelineCompletionIntentSchema); err != nil {
		return err
	}
	for _, trigger := range pipelineCompletionIntentTriggers {
		if err := checkObject(ctx, tx, "trigger", trigger.name, policyCompletionTriggerV62(trigger.sql)); err != nil {
			return err
		}
	}
	return nil
}
