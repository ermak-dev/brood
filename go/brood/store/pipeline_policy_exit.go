package store

import (
	"context"
	"errors"

	"brood.dev/go/brood/workflowpolicy"
)

// ErrNoPolicyExit means this reported visit has no supported terminal candidate.
var ErrNoPolicyExit = errors.New("policy result does not target an exit")

// PipelinePolicyTransitionResolution separates policy rejection from the actor's outcome.
// Applied is true only for a new committed transition, never a replay.
type PipelinePolicyTransitionResolution struct {
	CandidateID       string
	Decision          workflowpolicy.Decision
	Applied, Rejected bool
}

// PipelinePolicyExitResolution is the terminal transition result.
type PipelinePolicyExitResolution = PipelinePolicyTransitionResolution

// ResolvePipelinePolicyExit applies only a verified actor result targeting exit
// or a terminal no-edge failure. Neither case authorizes a successor actor.
// It performs no guest effect and never substitutes a policy rejection for the
// actor's reported outcome. Callers must establish current external actor scope.
func (s *Store) ResolvePipelinePolicyExit(ctx context.Context, swarmID string, sequence int64) (PipelinePolicyExitResolution, error) {
	return s.resolvePipelinePolicyTransition(ctx, swarmID, sequence, "exit", ErrNoPolicyExit)
}

func (s *Store) resolvePipelinePolicyTransition(ctx context.Context, swarmID string, sequence int64, targetKind string, unsupported error) (PipelinePolicyTransitionResolution, error) {
	var result PipelinePolicyTransitionResolution
	err := s.WithTx(ctx, func(ctx context.Context) error {
		intent, err := s.GetPipelineCompletionIntent(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		visit, err := s.GetSwarmStep(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		if visit.State == "completed" {
			if visit.Outcome == nil || *visit.Outcome != intent.Outcome || visit.Report != intent.Report {
				return ErrPipelineVisitConflict
			}
			return nil
		}
		negative, err := s.ResolvePipelinePolicyReview(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		if negative.Applied {
			result = negative
			return nil
		}
		preview, err := s.PreviewActorResultPolicy(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		if preview.Input.TargetKind != targetKind {
			return unsupported
		}
		candidate, err := s.RecordPipelinePolicyCandidate(ctx, swarmID, sequence, "result", NewID())
		if err != nil {
			return err
		}
		result.CandidateID = candidate.ID
		result.Decision, result.Rejected, err = s.pipelinePolicyCandidateDecision(ctx, candidate)
		if err != nil {
			return err
		}
		if result.Decision.State == workflowpolicy.Denied {
			result.Rejected = true
		}
		if !result.Rejected && result.Decision.State != workflowpolicy.Allowed && !s.pipelinePolicyReviewEntryAllowed(ctx, candidate, result.Decision) {
			return nil
		}
		w, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		if result.Rejected {
			now := nowRFC3339()
			detail := policyRejectionReason(result.Decision)
			_, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='completed',outcome=?,report=NULLIF(?,''),finished_at=?,detail=? WHERE swarm_id=? AND sequence=?`, intent.Outcome, intent.Report, now, detail, swarmID, sequence)
			if err != nil {
				return err
			}
			if err := s.advanceExecution(ctx, swarmID, "failed", now); err != nil {
				return err
			}
			if err := s.queueTelegram(ctx, "pass_finished", TelegramEvent{Swarm: swarmID, Sequence: intent.Pass, Node: visit.NodeID, Execution: "failed"}); err != nil {
				return err
			}
		} else {
			if err := s.retainInlinePolicyReviewEvidence(ctx, swarmID, sequence); err != nil {
				return err
			}
			checked, err := s.checkActorCompletion(context.WithValue(ctx, childJoinCompletionKey{}, true), intent.ActorCompletion)
			if err != nil || checked.replay {
				if err != nil {
					return err
				}
				return ErrPipelineVisitConflict
			}
			if err := s.progressPipeline(ctx, w, checked.graph, sequence, visit.NodeID, "completed", intent.Outcome, intent.Report); err != nil {
				return err
			}
			if targetKind == "actor" {
				if err := s.assignInlinePolicyReview(ctx, candidate, sequence+1); err != nil {
					return err
				}
			}
		}
		result.Applied = true
		return nil
	})
	if err != nil {
		return PipelinePolicyExitResolution{}, err
	}
	return result, nil
}
