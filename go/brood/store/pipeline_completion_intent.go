package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrPipelineCompletionPending means a retained outcome awaits evidence and policy.
var ErrPipelineCompletionPending = fmt.Errorf("%w: completion intent awaits result evidence and policy", ErrPipelineVisitConflict)

// PipelineCompletionIntent retains the exact reported outcome without advancing the graph.
// It is not an attestation, a final step outcome or permission to repeat external effects.
type PipelineCompletionIntent struct {
	ActorCompletion
	Pass       int64
	Origin     string
	BaseCommit string
	CreatedAt  string
}

// GetPipelineCompletionIntent reads retained material; callers enforce swarm visibility.
func (s *Store) GetPipelineCompletionIntent(ctx context.Context, swarmID string, sequence int64) (PipelineCompletionIntent, error) {
	result := PipelineCompletionIntent{ActorCompletion: ActorCompletion{SwarmID: swarmID, Sequence: sequence}}
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT pass_sequence,request_id,actor_uid,actor_incarnation,origin,base_commit,outcome,report,created_at FROM pipeline_completion_intents WHERE swarm_id=? AND sequence=?`, swarmID, sequence).Scan(&result.Pass, &result.RequestID, &result.ActorUID, &result.Incarnation, &result.Origin, &result.BaseCommit, &result.Outcome, &result.Report, &result.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelineCompletionIntent{}, ErrNotFound
	}
	if err != nil {
		return PipelineCompletionIntent{}, err
	}
	return result, nil
}

// RecordPipelineCompletionIntent atomically rechecks authority and retains a report.
// authorize must validate the current authenticated actor request in the transaction.
// No observation, quiescence, notification or graph transition happens here.
func (s *Store) RecordPipelineCompletionIntent(ctx context.Context, in ActorCompletion, authorize func(context.Context) bool) (PipelineCompletionIntent, error) {
	if err := validActorCompletion(in); err != nil {
		return PipelineCompletionIntent{}, err
	}
	var result PipelineCompletionIntent
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrSwarmRequestDenied
		}
		checked, err := s.checkActorCompletion(ctx, in)
		if err != nil {
			return err
		}
		existing, err := s.GetPipelineCompletionIntent(ctx, in.SwarmID, in.Sequence)
		if err == nil {
			if existing.ActorCompletion != in {
				return ErrPipelineVisitConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if checked.replay {
			return ErrPipelineVisitConflict
		}
		pin, err := s.SwarmStepWorkflowPolicy(ctx, in.SwarmID, in.Sequence)
		if err != nil {
			return err
		}
		source, err := s.PipelineActorStepSource(ctx, in.SwarmID, in.Sequence)
		base := source.Commit
		if err != nil {
			return err
		}
		if checked.swarm.Source == nil || base == "" || checked.visit.AttestedCommit != base {
			return ErrPipelineSourceUnverified
		}
		if pin.Pass != checked.visit.Pass || pin.ProjectID != checked.swarm.ProjectID {
			return ErrPipelineVisitConflict
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_completion_intents(swarm_id,sequence,pass_sequence,request_id,actor_uid,actor_incarnation,origin,base_commit,outcome,report,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, in.SwarmID, in.Sequence, pin.Pass, in.RequestID, in.ActorUID, in.Incarnation, checked.swarm.Source.Origin, base, in.Outcome, in.Report, nowRFC3339())
		if err != nil {
			return err
		}
		result, err = s.GetPipelineCompletionIntent(ctx, in.SwarmID, in.Sequence)
		return err
	})
	if err != nil {
		return PipelineCompletionIntent{}, err
	}
	return result, nil
}

func (s *Store) checkPendingCompletionIntent(ctx context.Context, in ActorCompletion) error {
	intent, err := s.GetPipelineCompletionIntent(ctx, in.SwarmID, in.Sequence)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if intent.ActorCompletion != in {
		return ErrPipelineVisitConflict
	}
	return ErrPipelineCompletionPending
}
