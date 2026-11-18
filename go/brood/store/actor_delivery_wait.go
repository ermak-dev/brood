package store

import (
	"context"
	"strconv"
	"unicode/utf8"
)

// RecordActorDeliveryWait projects retained instruction evidence without granting
// another send. Known not-sent remains deliverable; unknown never resets to it.
func (s *Store) RecordActorDeliveryWait(ctx context.Context, id, detail string, authorize func(context.Context) bool) error {
	if len(detail) > 512 || !utf8.ValidString(detail) {
		return ErrActorDeliveryConflict
	}
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrActorDeliveryDenied
		}
		intent, err := s.GetActorDelivery(ctx, id)
		if err != nil {
			return err
		}
		if intent.Input.SourceKind != "step" {
			return ErrActorDeliveryConflict
		}
		if _, err := s.actorDeliveryCurrent(ctx, intent.Input); err != nil {
			return err
		}
		history, err := s.actorDeliveryHistory(ctx, id)
		if err != nil {
			return err
		}
		schedule := scheduleActorDelivery(history)
		if schedule.Confirmed {
			return ErrActorDeliveryComplete
		}
		sequence, err := strconv.ParseInt(intent.Input.SourceID, 10, 64)
		if err != nil {
			return ErrActorDeliveryConflict
		}
		var before string
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT state FROM swarm_steps WHERE swarm_id=? AND sequence=?`, intent.Input.SwarmID, sequence).Scan(&before); err != nil {
			return err
		}
		uncertain := intent.ProbeOnly || before == "unconfirmed"
		for _, entry := range history {
			if entry.Attempt.Mode == "send" && entry.Outcome != "not_sent" {
				uncertain = true
			}
		}
		after := before
		var finished any
		if uncertain {
			after = "unconfirmed"
		} else if schedule.Exhausted {
			after = "refused"
			finished = nowRFC3339()
		}
		if schedule.Exhausted {
			detail = ErrActorDeliveryBudget.Error()
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state=?,detail=?,finished_at=COALESCE(?,finished_at) WHERE swarm_id=? AND sequence=? AND request_id=?`, after, stepDetail(detail), finished, intent.Input.SwarmID, sequence, intent.Input.RequestID)
		if err != nil {
			return err
		}
		if after != before && (after == "unconfirmed" || after == "refused") {
			return s.queueStepFailure(ctx, intent.Input.SwarmID, sequence)
		}
		return nil
	})
}
