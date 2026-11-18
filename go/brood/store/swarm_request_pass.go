package store

import (
	"context"
	"database/sql"
	"errors"
)

// SwarmRequestPassContext records when an actor question was created during a
// pass. ObservedSequence is the then-current visit, not ownership of that visit
// by the asking actor. The context never grants replay or execution authority.
type SwarmRequestPassContext struct {
	SwarmID, RequestID     string
	Pass, ObservedSequence int64
}

// GetSwarmRequestPassContext reads retained context only within its exact swarm.
// Missing context means unscoped or legacy history, never an inferred pass.
// Trusted adapters must enforce current swarm visibility before exposing it.
func (s *Store) GetSwarmRequestPassContext(ctx context.Context, swarmID, requestID string) (SwarmRequestPassContext, error) {
	if !validPipelineField(swarmID) || !validPipelineField(requestID) {
		return SwarmRequestPassContext{}, ErrNotFound
	}
	var result SwarmRequestPassContext
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT swarm_id,request_id,pass_sequence,observed_sequence FROM swarm_request_pass_contexts WHERE swarm_id=? AND request_id=?`, swarmID, requestID).Scan(&result.SwarmID, &result.RequestID, &result.Pass, &result.ObservedSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return SwarmRequestPassContext{}, ErrNotFound
	}
	return result, err
}

func (s *Store) captureSwarmRequestPassContext(ctx context.Context, swarmID, requestID string) error {
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO swarm_request_pass_contexts(request_id,swarm_id,pass_sequence,observed_sequence)
 SELECT r.id,r.swarm_id,p.sequence,st.sequence
 FROM swarm_requests r JOIN swarms sw ON sw.id=r.swarm_id
 JOIN swarm_steps st ON st.swarm_id=sw.id AND st.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=sw.id)
 JOIN swarm_steps p ON p.swarm_id=sw.id AND p.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=sw.id AND goal IS NOT NULL)
 WHERE r.id=? AND r.swarm_id=? AND r.origin='actor' AND sw.state='active' AND sw.execution IN ('running','waiting')`, requestID, swarmID)
	return err
}
