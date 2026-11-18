package store

import (
	"context"
	"database/sql"
	"errors"
)

// LatestSwarmEvalReplay returns the most recently appended local decision for
// status display. The retained record never grants authority to resume execution.
func (s *Store) LatestSwarmEvalReplay(ctx context.Context, swarmID string) (SwarmEvalReplay, error) {
	var requestID string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT request_id FROM swarm_eval_replays WHERE candidate_swarm_id=? ORDER BY rowid DESC LIMIT 1`, swarmID).Scan(&requestID)
	if errors.Is(err, sql.ErrNoRows) {
		return SwarmEvalReplay{}, ErrNotFound
	}
	if err != nil {
		return SwarmEvalReplay{}, err
	}
	return s.GetSwarmEvalReplay(ctx, swarmID, requestID)
}

// WaitingSwarmEvalSwarms returns a bounded batch of active evals with an open
// retained question after the stable swarm ID cursor. It performs no mutation
// and never selects stopped evals.
func (s *Store) WaitingSwarmEvalSwarms(ctx context.Context, after string) ([]string, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT s.id FROM swarms s JOIN swarm_eval_bindings b ON b.candidate_swarm_id=s.id WHERE s.id>? AND s.execution IN ('running','waiting') AND EXISTS(SELECT 1 FROM swarm_requests r WHERE r.swarm_id=s.id AND r.state='open') ORDER BY s.id LIMIT 256`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
