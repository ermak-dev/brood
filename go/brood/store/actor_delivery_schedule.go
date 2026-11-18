package store

import (
	"context"
	"errors"
	"time"
)

// ActorDeliverySchedule is retained timing information, never send permission.
type ActorDeliverySchedule struct {
	Confirmed, Exhausted, UncertainSend bool
	NotBefore                           time.Time
}

func scheduleActorDelivery(history []ActorDeliveryObservation) ActorDeliverySchedule {
	var schedule ActorDeliverySchedule
	for _, entry := range history {
		if entry.Outcome == "delivered" || entry.Outcome == "duplicate" {
			return ActorDeliverySchedule{Confirmed: true}
		}
		if entry.Attempt.Mode == "send" && (entry.Outcome == "" || entry.Outcome == "unconfirmed") {
			schedule.UncertainSend = true
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

// ReconcileExhaustedActorDeliveryAnswer records uncertainty from the retained
// ledger after the last ticket's deadline and backoff. It does not reserve an
// attempt or need a live receiver. A known not-sent result is not downgraded.
func (s *Store) ReconcileExhaustedActorDeliveryAnswer(ctx context.Context, id string, now time.Time, authorize func(context.Context) bool) (SwarmRequest, error) {
	var request SwarmRequest
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrActorDeliveryDenied
		}
		intent, err := s.GetActorDelivery(ctx, id)
		if err != nil {
			return err
		}
		if intent.Input.SourceKind != "answer" {
			return ErrActorDeliveryConflict
		}
		if _, err := s.actorDeliverySource(ctx, intent.Input, true); err != nil {
			return err
		}
		history, err := s.actorDeliveryHistory(ctx, id)
		if err != nil {
			return err
		}
		request, err = s.GetSwarmRequest(ctx, intent.Input.SwarmID, intent.Input.SourceID)
		if err != nil {
			return err
		}
		schedule := scheduleActorDelivery(history)
		if !schedule.Exhausted || !schedule.UncertainSend || now.Before(schedule.NotBefore) || request.Delivery == "unconfirmed" || request.Delivery == "delivered" {
			return nil
		}
		request, err = s.RecordSwarmRequestDelivery(ctx, request.SwarmID, request.ID, "unconfirmed")
		return err
	})
	if err != nil {
		return SwarmRequest{}, err
	}
	return request, nil
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
 AND ((SELECT COUNT(*) FROM actor_delivery_attempts at WHERE at.delivery_id=d.id)<?
      OR EXISTS(SELECT 1 FROM actor_delivery_outcomes o WHERE o.delivery_id=d.id AND o.outcome IN ('delivered','duplicate'))
      OR (r.delivery<>'unconfirmed' AND EXISTS(
          SELECT 1 FROM actor_delivery_attempts at LEFT JOIN actor_delivery_outcomes o ON o.delivery_id=at.delivery_id AND o.number=at.number
          WHERE at.delivery_id=d.id AND at.mode='send' AND (o.outcome IS NULL OR o.outcome='unconfirmed'))))
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
