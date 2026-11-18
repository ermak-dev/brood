package store

import (
	"context"
	"errors"
)

// PendingActorDeliverySteps pages only exact retained instruction sources.
// Unknown attestation or legacy delivery without an intent is never reconstructed.
func (s *Store) PendingActorDeliverySteps(ctx context.Context, after string, limit int) ([]ActorDeliveryCandidate, error) {
	if limit < 1 || limit > 64 {
		return nil, errors.New("delivery candidate page size must be between 1 and 64")
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT d.id,d.swarm_id,d.source_id FROM actor_deliveries d
 JOIN swarm_steps st ON st.swarm_id=d.swarm_id AND CAST(st.sequence AS TEXT)=d.source_id
 JOIN swarms sw ON sw.id=d.swarm_id
 JOIN managed_actors a ON a.uid=d.actor_uid AND a.swarm_id=d.swarm_id AND a.incarnation=d.incarnation AND a.atespace_uid=d.atespace_uid
 WHERE d.source_kind='step' AND d.id>? AND st.state IN ('delivering','unconfirmed') AND st.request_id=d.request_id AND st.attested_commit=d.attested_commit AND st.actor_uid=d.actor_uid AND st.actor_incarnation=d.incarnation
 AND st.sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=d.swarm_id)
 AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions WHERE swarm_id=d.swarm_id AND sequence=st.sequence)
 AND sw.state='active' AND sw.execution IN ('running','waiting') AND sw.atespace_uid=d.atespace_uid AND a.desired='running' AND a.phase IN ('provisioning','running') AND a.stage<>'stop'
 AND ((SELECT COUNT(*) FROM actor_delivery_attempts at WHERE at.delivery_id=d.id)<8 OR EXISTS(SELECT 1 FROM actor_delivery_outcomes o WHERE o.delivery_id=d.id AND o.outcome IN ('delivered','duplicate')) OR COALESCE(st.detail,'')<>?)
 ORDER BY d.id LIMIT ?`, after, ErrActorDeliveryBudget.Error(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActorDeliveryCandidate{}
	for rows.Next() {
		var candidate ActorDeliveryCandidate
		if err := rows.Scan(&candidate.ID, &candidate.SwarmID, &candidate.SourceID); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}
