package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrPipelineCompletionReceiptPending means the actor client has not acknowledged acceptance.
var ErrPipelineCompletionReceiptPending = fmt.Errorf("%w: completion acceptance has no client receipt", ErrPipelineVisitConflict)

// PipelineCompletionReceipt retains acceptance and optional client acknowledgement.
// Acknowledgement proves reading acceptance, not completion or transition permission.
type PipelineCompletionReceipt struct {
	Version        int
	Intent         PipelineCompletionIntent
	Nonce          string
	CreatedAt      string
	AcknowledgedAt string
}

// PipelineCompletionAcknowledgement is filled from an authenticated actor principal
// and a supervisor-forwarded receipt, never from actor-selected sender identity.
type PipelineCompletionAcknowledgement struct {
	Version     int
	SwarmID     string
	Pass        int64
	Sequence    int64
	RequestID   string
	ActorUID    string
	Incarnation int64
	Nonce       string
}

func validCompletionNonce(nonce string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(nonce)
	return err == nil && len(raw) == 32
}

// GetPipelineCompletionReceipt reads retained material; callers enforce swarm visibility.
func (s *Store) GetPipelineCompletionReceipt(ctx context.Context, swarmID string, sequence int64) (PipelineCompletionReceipt, error) {
	var result PipelineCompletionReceipt
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT r.version,r.nonce,r.created_at,COALESCE(a.acknowledged_at,'') FROM pipeline_completion_receipts r LEFT JOIN pipeline_completion_acknowledgements a ON a.swarm_id=r.swarm_id AND a.sequence=r.sequence WHERE r.swarm_id=? AND r.sequence=?`, swarmID, sequence).Scan(&result.Version, &result.Nonce, &result.CreatedAt, &result.AcknowledgedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	result.Intent, err = s.GetPipelineCompletionIntent(ctx, swarmID, sequence)
	return result, err
}

// AcceptPipelineCompletion atomically records intent and its acceptance receipt.
// The server generates an unpredictable nonce before the transaction; exact retries
// keep the stored nonce even if the caller generated another one. No guest RPC runs.
func (s *Store) AcceptPipelineCompletion(ctx context.Context, in ActorCompletion, nonce string, authorize func(context.Context) bool) (PipelineCompletionReceipt, error) {
	if !validCompletionNonce(nonce) {
		return PipelineCompletionReceipt{}, errors.New("invalid completion receipt nonce")
	}
	var result PipelineCompletionReceipt
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.RecordPipelineCompletionIntent(ctx, in, authorize); err != nil {
			return err
		}
		existing, err := s.GetPipelineCompletionReceipt(ctx, in.SwarmID, in.Sequence)
		if err == nil {
			result = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_completion_receipts(swarm_id,sequence,version,nonce,created_at) VALUES(?,?,1,?,?)`, in.SwarmID, in.Sequence, nonce, nowRFC3339())
		if err != nil {
			return err
		}
		result, err = s.GetPipelineCompletionReceipt(ctx, in.SwarmID, in.Sequence)
		return err
	})
	if err != nil {
		return PipelineCompletionReceipt{}, err
	}
	return result, nil
}

// AcknowledgePipelineCompletion records one exact authenticated acceptance receipt.
// The callback rechecks current actor authority in the transaction. Duplicate ACKs
// return retained history, never new permission to advance or repeat an observation.
func (s *Store) AcknowledgePipelineCompletion(ctx context.Context, in PipelineCompletionAcknowledgement, authorize func(context.Context) bool) (PipelineCompletionReceipt, error) {
	if !validCompletionNonce(in.Nonce) {
		return PipelineCompletionReceipt{}, errors.New("invalid completion receipt nonce")
	}
	var result PipelineCompletionReceipt
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrSwarmRequestDenied
		}
		receipt, err := s.GetPipelineCompletionReceipt(ctx, in.SwarmID, in.Sequence)
		if err != nil {
			return err
		}
		intent := receipt.Intent
		if receipt.Version != in.Version || receipt.Nonce != in.Nonce || intent.Pass != in.Pass || intent.RequestID != in.RequestID || intent.ActorUID != in.ActorUID || intent.Incarnation != in.Incarnation {
			return ErrPipelineVisitConflict
		}
		if receipt.AcknowledgedAt != "" {
			result = receipt
			return nil
		}
		checked, err := s.checkActorCompletion(context.WithValue(ctx, childJoinCompletionKey{}, true), intent.ActorCompletion)
		if err != nil {
			return err
		}
		if checked.replay {
			return ErrPipelineVisitConflict
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_completion_acknowledgements(swarm_id,sequence,acknowledged_at) VALUES(?,?,?)`, in.SwarmID, in.Sequence, nowRFC3339())
		if err != nil {
			return err
		}
		result, err = s.GetPipelineCompletionReceipt(ctx, in.SwarmID, in.Sequence)
		return err
	})
	if err != nil {
		return PipelineCompletionReceipt{}, err
	}
	return result, nil
}

func (s *Store) requireCompletionReceipt(ctx context.Context, intent PipelineCompletionIntent) error {
	receipt, err := s.GetPipelineCompletionReceipt(ctx, intent.SwarmID, intent.Sequence)
	if errors.Is(err, ErrNotFound) {
		return ErrPipelineCompletionReceiptPending
	}
	if err != nil {
		return err
	}
	if receipt.AcknowledgedAt == "" {
		return ErrPipelineCompletionReceiptPending
	}
	if receipt.Intent != intent {
		return ErrPipelineVisitConflict
	}
	return nil
}
