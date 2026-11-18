package store

import "strings"

// Версия 42 остаётся замороженной для цепочки миграций. Версия 62 добавляет
// entry после human: он может нести последний verified artifact того же pass.
func pipelinePolicyCandidateTableV62() string {
	table := strings.Replace(pipelinePolicyCandidateSchema, "CHECK ((boundary='entry' AND evidence_id IS NULL) OR (boundary='result' AND evidence_id IS NOT NULL))", "CHECK (boundary='entry' OR evidence_id IS NOT NULL)", 1)
	return strings.Replace(table, "CHECK (json_extract(preview,'$.Input.Initial') IS (boundary='entry'))", "CHECK (json_extract(preview,'$.Input.Initial') IS (evidence_id IS NULL))", 1)
}

func pipelinePolicyCandidateTriggerV62(sql string) string {
	return strings.Replace(sql,
		"OR (NEW.boundary='result' AND st.state IN ('inflight','delivering','unconfirmed') AND EXISTS(",
		`OR (NEW.boundary='entry' AND st.sequence>NEW.pass_sequence AND st.state='pending' AND st.actor_uid IS NULL AND st.request_id IS NULL
  AND EXISTS(SELECT 1 FROM swarm_steps h WHERE h.swarm_id=st.swarm_id AND h.sequence=st.sequence-1 AND h.state='answered')
  AND ((NEW.evidence_id IS NULL AND NOT EXISTS(SELECT 1 FROM pipeline_completion_intents WHERE swarm_id=NEW.swarm_id AND pass_sequence=NEW.pass_sequence))
  OR EXISTS(SELECT 1 FROM pipeline_result_observations o JOIN pipeline_completion_intents i ON i.swarm_id=o.swarm_id AND i.sequence=o.sequence JOIN pipeline_result_evidence e ON e.observation_id=o.id JOIN swarm_steps a ON a.swarm_id=i.swarm_id AND a.sequence=i.sequence
  WHERE o.id=NEW.evidence_id AND o.swarm_id=NEW.swarm_id AND o.state='observed' AND i.pass_sequence=NEW.pass_sequence AND a.state='completed'
  AND i.sequence=(SELECT MAX(sequence) FROM pipeline_completion_intents WHERE swarm_id=NEW.swarm_id AND pass_sequence=NEW.pass_sequence AND sequence<NEW.sequence)
  AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions WHERE swarm_id=a.swarm_id AND sequence=a.sequence))))
 OR (NEW.boundary='result' AND st.state IN ('inflight','delivering','unconfirmed') AND EXISTS(`, 1)
}

func pipelinePolicyCandidateFullSchemaV62() string {
	statements := []string{pipelinePolicyCandidateTableV62()}
	for _, trigger := range pipelinePolicyCandidateTriggers {
		statements = append(statements, pipelinePolicyCandidateTriggerV62(trigger.sql))
	}
	return strings.Join(statements, ";")
}

// Только версия 62 меняет source fence: assignment закреплён за следующим
// inline visit. Старые DDL нужны миграциям 26–61 без каких-либо изменений.
const inlineReviewCommitSQL = `(SELECT r.artifact FROM pipeline_policy_review_assignments a JOIN pipeline_policy_review_requirements r ON r.candidate_id=a.candidate_id AND r.rule_id=a.rule_id WHERE a.review_swarm_id=NEW.swarm_id AND a.review_sequence=NEW.sequence ORDER BY a.rule_id LIMIT 1)`

func policyStepSourceTriggerV62(ddl string) string {
	return strings.Replace(ddl, swarmStepPassCommit, "COALESCE("+inlineReviewCommitSQL+", "+swarmStepPassCommit+")", 1)
}
func policyCompletionTriggerV62(ddl string) string {
	return strings.Replace(ddl, "first.source_commit=NEW.base_commit", "COALESCE("+inlineReviewCommitSQL+", first.source_commit)=NEW.base_commit", 1)
}
func pipelineCompletionIntentFullSchemaV62() string {
	statements := []string{pipelineCompletionIntentSchema}
	for _, t := range pipelineCompletionIntentTriggers {
		statements = append(statements, policyCompletionTriggerV62(t.sql))
	}
	return strings.Join(statements, ";")
}

func pipelinePolicyProgressionMigration() string {
	statements := []string{
		"CREATE TEMP TABLE pipeline_policy_candidates_copy AS SELECT * FROM pipeline_policy_candidates",
		"DROP TABLE pipeline_policy_candidates", pipelinePolicyCandidateTableV62(),
		"INSERT INTO pipeline_policy_candidates SELECT * FROM pipeline_policy_candidates_copy",
		"DROP TABLE pipeline_policy_candidates_copy",
		pipelinePolicyReviewFullSchema(),
		"DROP TRIGGER swarm_steps_attested", policyStepSourceTriggerV62(swarmStepAttested),
		"DROP TRIGGER swarm_steps_attested_insert", policyStepSourceTriggerV62(swarmStepAttestedInsert),
		"DROP TRIGGER pipeline_completion_intents_insert", policyCompletionTriggerV62(pipelineCompletionIntentTriggers[0].sql),
	}
	for _, trigger := range pipelinePolicyCandidateTriggers {
		statements = append(statements, pipelinePolicyCandidateTriggerV62(trigger.sql))
	}
	return strings.Join(statements, ";")
}
