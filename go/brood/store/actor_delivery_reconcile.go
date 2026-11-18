package store

import (
	"context"
	"errors"
	"strconv"
)

// ErrActorDeliveryReceiptPending refuses source changes without retained proof.
var ErrActorDeliveryReceiptPending = errors.New("delivery has no retained confirmation")

// CheckActorDeliveryManager applies the canonical current manager rules using
// the supplied transaction context. It does not delegate external effects.
func (s *Store) CheckActorDeliveryManager(ctx context.Context, swarmID, userID string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.requireSwarmManager(ctx, swarmID, userID)
		return err
	})
}

// ReconcileActorDelivery applies only a retained delivered/duplicate observation
// to its still-current source. It never resends, advances a graph, or recreates
// the workspace attestation lease that ended before instruction delivery.
func (s *Store) ReconcileActorDelivery(ctx context.Context, id string, authorize func(context.Context) bool) error {
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrActorDeliveryDenied
		}
		intent, err := s.GetActorDelivery(ctx, id)
		if err != nil {
			return err
		}
		history, err := s.actorDeliveryHistory(ctx, id)
		if err != nil {
			return err
		}
		confirmed := false
		for _, entry := range history {
			if entry.Outcome == "delivered" || entry.Outcome == "duplicate" {
				confirmed = true
				break
			}
		}
		if !confirmed {
			return ErrActorDeliveryReceiptPending
		}
		if _, err := s.actorDeliverySource(ctx, intent.Input, true); err != nil {
			return err
		}
		if intent.Input.SourceKind == "child" {
			return nil
		}
		if intent.Input.SourceKind == "answer" {
			r, err := s.GetSwarmRequest(ctx, intent.Input.SwarmID, intent.Input.SourceID)
			if err != nil {
				return err
			}
			if r.Delivery == "delivered" {
				return nil
			}
			_, err = s.RecordSwarmRequestDelivery(ctx, r.SwarmID, r.ID, "delivered")
			return err
		}
		sequence, err := strconv.ParseInt(intent.Input.SourceID, 10, 64)
		if err != nil {
			return ErrActorDeliveryConflict
		}
		var state string
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT state FROM swarm_steps WHERE swarm_id=? AND sequence=?`, intent.Input.SwarmID, sequence).Scan(&state); err != nil {
			return err
		}
		if state == "inflight" {
			return nil
		}
		attempt := ActorStepAttempt{SwarmID: intent.Input.SwarmID, Sequence: sequence, RequestID: intent.Input.RequestID}
		return s.moveAttempt(ctx, attempt, []string{"delivering", "unconfirmed"}, "inflight", "", false)
	})
}
