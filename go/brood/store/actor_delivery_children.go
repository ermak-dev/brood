package store

import (
	"context"
	"errors"
)

// PendingActorDeliveryChildren pages retained typed child instructions only.
// Candidates are not dispatch authority: each send/probe must recheck current
// scope, manager authority, namespace and the original live journal. Confirmed,
// exhausted and completed work is excluded without releasing any child hold.
func (s *Store) PendingActorDeliveryChildren(ctx context.Context, after string, limit int) ([]ActorDeliveryCandidate, error) {
	if limit < 1 || limit > 64 {
		return nil, errors.New("delivery candidate page size must be between 1 and 64")
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT d.id,d.swarm_id,d.source_id FROM actor_deliveries d
 JOIN swarm_child_tasks c ON c.id=d.source_id AND c.swarm_id=d.swarm_id AND c.request_id=d.request_id AND c.actor_uid=d.actor_uid AND c.actor_incarnation=d.incarnation
 JOIN swarms sw ON sw.id=d.swarm_id
 JOIN managed_actors a ON a.uid=d.actor_uid AND a.swarm_id=d.swarm_id AND a.incarnation=d.incarnation AND a.atespace_uid=d.atespace_uid
 WHERE d.source_kind='child' AND d.id>? AND c.state='running' AND c.role='' AND c.delivery=''
 AND sw.state='active' AND sw.execution IN ('running','waiting') AND sw.atespace_uid=d.atespace_uid
 AND a.actor_class='ephemeral' AND a.desired='running' AND a.phase='running' AND a.operation='start' AND a.stage='done'
 AND NOT EXISTS(SELECT 1 FROM ephemeral_child_executor_failures f WHERE f.child_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM actor_delivery_outcomes o WHERE o.delivery_id=d.id AND o.outcome IN ('delivered','duplicate'))
 AND (SELECT count(*) FROM actor_delivery_attempts at WHERE at.delivery_id=d.id)<?
 ORDER BY d.id LIMIT ?`, after, MaxActorDeliveryAttempts, limit)
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
