package store

import (
	"context"
	"errors"
	"time"
)

// ActorDeliverySchedule is retained timing information, never send permission.
type ActorDeliverySchedule struct {
	Confirmed, Exhausted bool
	NotBefore            time.Time
}

func scheduleActorDelivery(history []ActorDeliveryObservation) ActorDeliverySchedule {
	var schedule ActorDeliverySchedule
	for _, entry := range history {
		if entry.Outcome == "delivered" || entry.Outcome == "duplicate" {
			schedule.Confirmed = true
			return schedule
		}
	}
	schedule.Exhausted = len(history) >= MaxActorDeliveryAttempts
	if len(history) > 0 {
		last := history[len(history)-1]
		anchor := last.ObservedAt
		if anchor.IsZero() {
			anchor = last.Attempt.DeadlineAt
		}
		schedule.NotBefore = anchor.Add(actorDeliveryBackoff(last.Attempt.Number))
	}
	return schedule
}

// GetActorDeliverySchedule reads a consistent deadline/backoff snapshot.
// Reservation still rechecks all timing and authority in its durable transaction.
func (s *Store) GetActorDeliverySchedule(ctx context.Context, id string) (ActorDeliverySchedule, error) {
	var out ActorDeliverySchedule
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetActorDelivery(ctx, id); err != nil {
			return err
		}
		history, err := s.actorDeliveryHistory(ctx, id)
		if err != nil {
			return err
		}
		out = scheduleActorDelivery(history)
		return nil
	})
	return out, err
}

// ActorDeliveryCandidate identifies one retained logical delivery awaiting recovery.
type ActorDeliveryCandidate struct{ ID, SwarmID, SourceID string }

// PendingActorDeliveryAnswers pages eligible retained intents, not legacy answers.
// A cursor is exclusive; empty pages let the caller wrap to the beginning.
func (s *Store) PendingActorDeliveryAnswers(ctx context.Context, after string, limit int) ([]ActorDeliveryCandidate, error) {
	if limit < 1 || limit > 64 {
		return nil, errors.New("delivery candidate page size must be between 1 and 64")
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT d.id,d.swarm_id,d.source_id FROM actor_deliveries d
 JOIN swarm_requests r ON r.swarm_id=d.swarm_id AND r.id=d.source_id
 JOIN swarms sw ON sw.id=d.swarm_id
 JOIN managed_actors a ON a.uid=d.actor_uid AND a.swarm_id=d.swarm_id AND a.incarnation=d.incarnation AND a.atespace_uid=d.atespace_uid
 WHERE d.source_kind='answer' AND d.id>? AND r.origin='actor' AND r.state='answered' AND r.actor_uid=d.actor_uid AND r.actor_incarnation=d.incarnation AND r.delivery IN ('pending','failed','unconfirmed')
 AND sw.state='active' AND sw.atespace_uid=d.atespace_uid AND a.desired='running' AND a.phase IN ('provisioning','running') AND a.stage<>'stop'
 AND ((SELECT COUNT(*) FROM actor_delivery_attempts at WHERE at.delivery_id=d.id)<8 OR EXISTS(SELECT 1 FROM actor_delivery_outcomes o WHERE o.delivery_id=d.id AND o.outcome IN ('delivered','duplicate')))
 ORDER BY d.id LIMIT ?`, after, limit)
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
