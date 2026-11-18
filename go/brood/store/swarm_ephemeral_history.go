package store

import "context"

// GetEphemeralChildIntentForActor reads immutable allocation history for an exact
// actor incarnation, including after inventory removal. This internal record
// contains the child payload: adapters must not expose it as a bootstrap DTO.
// It does not establish current authority, readiness, proof or effect permission.
func (s *Store) GetEphemeralChildIntentForActor(ctx context.Context, uid string, incarnation int64) (EphemeralChildIntent, error) {
	return scanEphemeralChildIntent(s.conn(ctx).QueryRowContext(ctx, `SELECT rowid,`+ephemeralChildIntentCols+` FROM ephemeral_child_intents WHERE id=(SELECT intent_id FROM ephemeral_child_allocations WHERE actor_uid=? AND actor_incarnation=?)`, uid, incarnation))
}

// GetEphemeralChildIntentForTask reads retained proof-bound membership, not live
// inventory or dispatch authority. Ordinary ad-hoc children have no such mapping.
// Adapters must separately authorize the caller and enforce current execution scope.
func (s *Store) GetEphemeralChildIntentForTask(ctx context.Context, id string) (EphemeralChildIntent, error) {
	return scanEphemeralChildIntent(s.conn(ctx).QueryRowContext(ctx, `SELECT rowid,`+ephemeralChildIntentCols+` FROM ephemeral_child_intents WHERE id=(
 SELECT e.id FROM swarm_child_tasks c
 JOIN ephemeral_child_allocations a ON a.actor_uid=c.actor_uid AND a.actor_incarnation=c.actor_incarnation
 JOIN ephemeral_child_intents e ON e.id=a.intent_id AND e.swarm_id=c.swarm_id AND e.parent_sequence=c.parent_sequence AND e.request_id=c.request_id AND e.payload=c.payload
 JOIN ephemeral_child_attestations p ON p.intent_id=e.id AND p.state='attested' AND p.actor_uid=a.actor_uid AND p.actor_incarnation=a.actor_incarnation
 AND p.origin=e.origin AND p.source_commit=e.source_commit AND p.attested_origin=e.origin AND p.attested_commit=e.source_commit
 WHERE c.id=? AND c.role='' AND c.delivery='')`, id))
}
