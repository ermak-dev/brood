package store

import (
	"context"
	"database/sql"
	"errors"
)

const pendingPipelineResultIntentSQL = `FROM pipeline_completion_intents i
 JOIN pipeline_result_observations o ON o.swarm_id=i.swarm_id AND o.sequence=i.sequence
 JOIN swarm_steps st ON st.swarm_id=i.swarm_id AND st.sequence=i.sequence
 JOIN swarms sw ON sw.id=i.swarm_id
 WHERE sw.state='active' AND sw.execution IN ('running','waiting')
 AND st.state IN ('inflight','delivering','unconfirmed') AND st.request_id=i.request_id AND st.actor_uid=i.actor_uid AND st.actor_incarnation=i.actor_incarnation
 AND st.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=i.swarm_id)
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions WHERE swarm_id=i.swarm_id AND sequence=i.sequence)`

const pendingObservedPipelineIntentSQL = pendingPipelineResultIntentSQL + ` AND o.state='observed'`

const pendingSettledPipelineIntentSQL = pendingPipelineResultIntentSQL + `
 AND (o.state='observed' OR (o.state='refused' AND EXISTS (
 SELECT 1 FROM pipeline_policy_review_assignments a WHERE a.review_swarm_id=i.swarm_id AND a.review_sequence=i.sequence)))`

// PendingObservedPipelineIntent selects current verified results for bookkeeping,
// not permission to run another observation or apply a transition.
func (s *Store) PendingObservedPipelineIntent(ctx context.Context, swarmID string) (PipelineCompletionIntent, error) {
	var sequence int64
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT i.sequence `+pendingObservedPipelineIntentSQL+` AND i.swarm_id=? LIMIT 1`, swarmID).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelineCompletionIntent{}, ErrNotFound
	}
	if err != nil {
		return PipelineCompletionIntent{}, err
	}
	return s.GetPipelineCompletionIntent(ctx, swarmID, sequence)
}

// PendingObservedPipelineSwarms resumes verified results and known inline review
// refusals without rerunning effects. A refusal is never returned as verified evidence.
func (s *Store) PendingObservedPipelineSwarms(ctx context.Context) ([]string, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT DISTINCT i.swarm_id `+pendingSettledPipelineIntentSQL+` ORDER BY i.swarm_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}
