package store

import (
	"context"
	"database/sql"
	"strings"
)

const pipelinePolicyReviewSchema = `CREATE TABLE pipeline_policy_review_requirements (
 candidate_id TEXT NOT NULL,
 rule_id TEXT NOT NULL,
 swarm_id TEXT NOT NULL,
 artifact TEXT NOT NULL CHECK (length(artifact)>0),
 producer_uid TEXT NOT NULL CHECK (length(producer_uid)>0),
 role TEXT NOT NULL CHECK (length(role)>0),
 PRIMARY KEY(candidate_id,rule_id),
 FOREIGN KEY(candidate_id) REFERENCES pipeline_policy_candidates(id)
);
CREATE TABLE pipeline_policy_review_assignments (
 candidate_id TEXT NOT NULL,
 rule_id TEXT NOT NULL,
 review_swarm_id TEXT NOT NULL,
 review_sequence INTEGER NOT NULL,
 actor_uid TEXT NOT NULL,
 incarnation INTEGER NOT NULL CHECK (incarnation>0),
 PRIMARY KEY(candidate_id,rule_id),
 FOREIGN KEY(candidate_id,rule_id) REFERENCES pipeline_policy_review_requirements(candidate_id,rule_id),
 FOREIGN KEY(review_swarm_id,review_sequence) REFERENCES swarm_steps(swarm_id,sequence)
);
CREATE TABLE pipeline_policy_review_evidence (
 candidate_id TEXT NOT NULL,
 rule_id TEXT NOT NULL,
 observation_id TEXT NOT NULL,
 PRIMARY KEY(candidate_id,rule_id),
 FOREIGN KEY(candidate_id,rule_id) REFERENCES pipeline_policy_review_assignments(candidate_id,rule_id),
 FOREIGN KEY(observation_id) REFERENCES pipeline_result_observations(id)
);`

func pipelinePolicyReviewObjects() []struct{ name, sql string } {
	objects := []struct{ name, sql string }{
		{"pipeline_policy_review_requirements_insert", `CREATE TRIGGER pipeline_policy_review_requirements_insert BEFORE INSERT ON pipeline_policy_review_requirements WHEN NOT EXISTS (
 SELECT 1 FROM pipeline_policy_candidates c,json_each(c.preview,'$.Policy.Rules') r
 WHERE c.id=NEW.candidate_id AND c.swarm_id=NEW.swarm_id AND (EXISTS(SELECT 1 FROM json_each(c.preview,'$.Decision.Review') d WHERE d.value=NEW.rule_id) OR (json_extract(c.preview,'$.Input.ActionsKnown')=1 AND json_extract(c.preview,'$.Input.Actions')='["review"]')) AND json_extract(r.value,'$.id')=NEW.rule_id AND json_extract(r.value,'$.require')='independent_review'
 AND json_extract(r.value,'$.reviewRole')=NEW.role AND json_extract(c.preview,'$.Input.Evidence.Artifact')=NEW.artifact AND json_extract(c.preview,'$.Input.Evidence.ProducerUID')=NEW.producer_uid
 ) BEGIN SELECT RAISE(ABORT,'review requirement must match exact candidate'); END`},
		{"pipeline_policy_review_assignments_insert", `CREATE TRIGGER pipeline_policy_review_assignments_insert BEFORE INSERT ON pipeline_policy_review_assignments WHEN NOT EXISTS (
 SELECT 1 FROM pipeline_policy_review_requirements r JOIN pipeline_policy_candidates c ON c.id=r.candidate_id JOIN swarms source ON source.id=r.swarm_id JOIN swarms review ON review.id=NEW.review_swarm_id JOIN swarm_steps st ON st.swarm_id=review.id AND st.sequence=NEW.review_sequence JOIN swarm_role_bindings rb ON rb.swarm_id=review.id AND rb.role=r.role
 WHERE r.candidate_id=NEW.candidate_id AND r.rule_id=NEW.rule_id AND source.project_id=review.project_id AND source.state='active' AND review.state='active' AND source.execution IN ('running','waiting') AND review.execution IN ('running','waiting')
 AND review.id=source.id AND st.sequence=c.sequence+CASE c.boundary WHEN 'result' THEN 1 ELSE 0 END AND st.node_id=json_extract(c.preview,'$.Transition.Next')
 AND c.pass_sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=st.swarm_id AND sequence<=st.sequence AND goal IS NOT NULL)
 AND st.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=st.swarm_id)
 AND json_extract(c.preview,'$.Input.ActionsKnown')=1 AND json_extract(c.preview,'$.Input.Actions')='["review"]'
 AND st.state='pending' AND NEW.actor_uid!=r.producer_uid AND rb.actor_uid=NEW.actor_uid AND rb.actor_incarnation=NEW.incarnation
 AND NOT EXISTS(SELECT 1 FROM pipeline_completion_intents WHERE swarm_id=review.id AND sequence=st.sequence)
 ) BEGIN SELECT RAISE(ABORT,'review assignment requires current independent role'); END`},
		{"pipeline_policy_review_evidence_insert", `CREATE TRIGGER pipeline_policy_review_evidence_insert BEFORE INSERT ON pipeline_policy_review_evidence WHEN NOT EXISTS (
 SELECT 1 FROM pipeline_policy_review_assignments a JOIN pipeline_policy_review_requirements r ON r.candidate_id=a.candidate_id AND r.rule_id=a.rule_id JOIN pipeline_result_observations o ON o.id=NEW.observation_id JOIN pipeline_result_evidence e ON e.observation_id=o.id JOIN pipeline_completion_intents i ON i.swarm_id=o.swarm_id AND i.sequence=o.sequence
 WHERE a.candidate_id=NEW.candidate_id AND a.rule_id=NEW.rule_id AND o.swarm_id=a.review_swarm_id AND o.sequence=a.review_sequence AND o.state='observed' AND i.actor_uid=a.actor_uid AND i.actor_incarnation=a.incarnation AND i.outcome='success' AND i.base_commit=r.artifact AND json_extract(e.attestation,'$.head')=r.artifact
 ) BEGIN SELECT RAISE(ABORT,'review evidence requires exact observed artifact'); END`},
	}
	for _, table := range []string{"pipeline_policy_review_requirements", "pipeline_policy_review_assignments", "pipeline_policy_review_evidence"} {
		objects = append(objects, struct{ name, sql string }{table + "_no_update", "CREATE TRIGGER " + table + "_no_update BEFORE UPDATE ON " + table + " BEGIN SELECT RAISE(ABORT,'review history is immutable'); END"}, struct{ name, sql string }{table + "_no_delete", "CREATE TRIGGER " + table + "_no_delete BEFORE DELETE ON " + table + " BEGIN SELECT RAISE(ABORT,'review history is immutable'); END"}, struct{ name, sql string }{table + "_no_replace", "CREATE TRIGGER " + table + "_no_replace BEFORE INSERT ON " + table + " WHEN EXISTS(SELECT 1 FROM " + table + " WHERE candidate_id=NEW.candidate_id AND rule_id=NEW.rule_id) BEGIN SELECT RAISE(ABORT,'review history is immutable'); END"})
	}
	return objects
}
func pipelinePolicyReviewFullSchema() string {
	statements := []string{pipelinePolicyReviewSchema}
	for _, o := range pipelinePolicyReviewObjects() {
		statements = append(statements, o.sql)
	}
	return strings.Join(statements, ";")
}
func verifyPipelinePolicyReviewSchema(ctx context.Context, tx *sql.Tx) error {
	for _, ddl := range strings.Split(pipelinePolicyReviewSchema, ";") {
		if strings.TrimSpace(ddl) != "" {
			if err := checkSchema(ctx, tx, ddl); err != nil {
				return err
			}
		}
	}
	for _, o := range pipelinePolicyReviewObjects() {
		if err := checkObject(ctx, tx, "trigger", o.name, o.sql); err != nil {
			return err
		}
	}
	return nil
}
