package store

import (
	"context"
	"database/sql"
	"errors"
)

const pendingPipelineResultFrom = ` FROM pipeline_completion_intents i
 JOIN pipeline_completion_acknowledgements a ON a.swarm_id=i.swarm_id AND a.sequence=i.sequence
 JOIN swarm_steps st ON st.swarm_id=i.swarm_id AND st.sequence=i.sequence
 JOIN swarms sw ON sw.id=i.swarm_id
 LEFT JOIN pipeline_result_observations o ON o.swarm_id=i.swarm_id AND o.sequence=i.sequence
 WHERE o.id IS NULL AND sw.state='active' AND sw.execution IN ('running','waiting')
 AND st.state IN ('inflight','delivering','unconfirmed') AND st.actor_uid=i.actor_uid AND st.actor_incarnation=i.actor_incarnation AND st.request_id=i.request_id AND st.attested_commit=i.base_commit
 AND st.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=i.swarm_id)
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions WHERE swarm_id=i.swarm_id AND sequence=i.sequence)`

// PendingPipelineResultIntent selects acknowledged, never-observed current work.
// Reservation repeats all identity/role/lease checks before any external operation.
func (s *Store) PendingPipelineResultIntent(ctx context.Context, swarmID string) (PipelineCompletionIntent, error) {
	var sequence int64
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT i.sequence`+pendingPipelineResultFrom+` AND i.swarm_id=?`, swarmID).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelineCompletionIntent{}, ErrNotFound
	}
	if err != nil {
		return PipelineCompletionIntent{}, err
	}
	return s.GetPipelineCompletionIntent(ctx, swarmID, sequence)
}

// PendingPipelineResultSwarms identifies safe-to-reserve work after receipt or restart.
// Existing observations, including refused and unconfirmed ones, are never requeued.
func (s *Store) PendingPipelineResultSwarms(ctx context.Context) ([]string, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT i.swarm_id`+pendingPipelineResultFrom+` ORDER BY i.swarm_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}
