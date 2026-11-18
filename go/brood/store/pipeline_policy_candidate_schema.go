package store

import (
	"context"
	"database/sql"
	"strings"
)

const pipelinePolicyCandidateSchema = `CREATE TABLE pipeline_policy_candidates (
 id TEXT NOT NULL PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 128),
 version INTEGER NOT NULL CHECK (version=1),
 swarm_id TEXT NOT NULL,
 sequence INTEGER NOT NULL CHECK (sequence > 0),
 pass_sequence INTEGER NOT NULL CHECK (pass_sequence > 0 AND pass_sequence <= sequence),
 boundary TEXT NOT NULL CHECK (boundary IN ('entry','result')),
 policy_revision INTEGER NOT NULL CHECK (policy_revision > 0),
 evidence_id TEXT,
 preview TEXT NOT NULL CHECK (length(CAST(preview AS BLOB)) <= 8388608 AND json_valid(preview) AND json_type(preview)='object'),
 created_at TEXT NOT NULL CHECK (length(created_at)>0),
 UNIQUE (swarm_id,sequence,boundary),
 FOREIGN KEY (swarm_id,pass_sequence) REFERENCES swarm_pass_policies(swarm_id,pass_sequence),
 FOREIGN KEY (evidence_id) REFERENCES pipeline_result_observations(id),
 CHECK ((boundary='entry' AND evidence_id IS NULL) OR (boundary='result' AND evidence_id IS NOT NULL)),
 CHECK (json_extract(preview,'$.SwarmID') IS swarm_id AND json_extract(preview,'$.Policy.SwarmID') IS swarm_id AND json_extract(preview,'$.Sequence') IS sequence AND json_extract(preview,'$.Policy.Pass') IS pass_sequence AND json_extract(preview,'$.Policy.Revision') IS policy_revision AND json_extract(preview,'$.EvidenceID') IS COALESCE(evidence_id,'')),
 CHECK (json_extract(preview,'$.Input.Initial') IS (boundary='entry')),
 CHECK (COALESCE(json_extract(preview,'$.Decision.State'),'') IN ('allowed','blocked','waiting_human','waiting_review','denied'))
);`

var pipelinePolicyCandidateTriggers = []struct{ name, sql string }{
	{"pipeline_policy_candidates_insert", `CREATE TRIGGER pipeline_policy_candidates_insert BEFORE INSERT ON pipeline_policy_candidates WHEN NOT EXISTS (
 SELECT 1 FROM swarm_steps st JOIN swarms sw ON sw.id=st.swarm_id JOIN swarm_pass_policies p ON p.swarm_id=st.swarm_id AND p.pass_sequence=NEW.pass_sequence
 WHERE st.swarm_id=NEW.swarm_id AND st.sequence=NEW.sequence AND p.policy_revision=NEW.policy_revision AND p.project_id=sw.project_id AND json_extract(NEW.preview,'$.Policy.ProjectID') IS p.project_id
 AND json_extract(NEW.preview,'$.Policy.LegacyEmpty') IS p.legacy_empty
 AND json_extract(NEW.preview,'$.Policy.Rules') IS (SELECT rules FROM project_workflow_policy_versions WHERE project_id=p.project_id AND revision=p.policy_revision)
 AND sw.state='active' AND sw.execution IN ('running','waiting') AND st.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=NEW.swarm_id)
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions WHERE swarm_id=NEW.swarm_id AND sequence=NEW.sequence)
 AND ((NEW.boundary='entry' AND st.sequence=NEW.pass_sequence AND st.goal IS NOT NULL AND st.state IN ('pending','waiting') AND st.actor_uid IS NULL AND st.request_id IS NULL AND NOT EXISTS(SELECT 1 FROM pipeline_completion_intents WHERE swarm_id=NEW.swarm_id AND pass_sequence=NEW.pass_sequence))
 OR (NEW.boundary='result' AND st.state IN ('inflight','delivering','unconfirmed') AND EXISTS(
 SELECT 1 FROM pipeline_result_observations o JOIN pipeline_completion_intents i ON i.swarm_id=o.swarm_id AND i.sequence=o.sequence JOIN pipeline_result_evidence e ON e.observation_id=o.id
 WHERE o.id=NEW.evidence_id AND o.swarm_id=NEW.swarm_id AND o.sequence=NEW.sequence AND o.state='observed' AND i.pass_sequence=NEW.pass_sequence AND i.actor_uid=st.actor_uid AND i.actor_incarnation=st.actor_incarnation AND i.request_id=st.request_id)))
 ) BEGIN SELECT RAISE(ABORT, 'policy candidate requires the current verified boundary'); END`},
	{"pipeline_policy_candidates_no_update", `CREATE TRIGGER pipeline_policy_candidates_no_update BEFORE UPDATE ON pipeline_policy_candidates BEGIN SELECT RAISE(ABORT, 'policy candidate history is immutable'); END`},
	{"pipeline_policy_candidates_no_delete", `CREATE TRIGGER pipeline_policy_candidates_no_delete BEFORE DELETE ON pipeline_policy_candidates BEGIN SELECT RAISE(ABORT, 'policy candidate history is immutable'); END`},
	{"pipeline_policy_candidates_no_replace", `CREATE TRIGGER pipeline_policy_candidates_no_replace BEFORE INSERT ON pipeline_policy_candidates WHEN EXISTS(SELECT 1 FROM pipeline_policy_candidates WHERE id=NEW.id OR (swarm_id=NEW.swarm_id AND sequence=NEW.sequence AND boundary=NEW.boundary)) BEGIN SELECT RAISE(ABORT, 'policy candidate history is immutable'); END`},
}

func pipelinePolicyCandidateFullSchema() string {
	statements := []string{pipelinePolicyCandidateSchema}
	for _, trigger := range pipelinePolicyCandidateTriggers {
		statements = append(statements, trigger.sql)
	}
	return strings.Join(statements, ";")
}

func verifyPipelinePolicyCandidateSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, pipelinePolicyCandidateTableV62()); err != nil {
		return err
	}
	for _, trigger := range pipelinePolicyCandidateTriggers {
		if err := checkObject(ctx, tx, "trigger", trigger.name, pipelinePolicyCandidateTriggerV62(trigger.sql)); err != nil {
			return err
		}
	}
	return nil
}
