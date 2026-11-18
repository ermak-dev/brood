package store

import (
	"context"
	"errors"
	"fmt"

	"brood.dev/go/brood/workflowpolicy"
)

// ResolvePipelinePolicyReview resolves a known negative inline review without
// inventing an actor outcome. It never retries/replaces an assignment. A refused
// instruction, verified negative/changed artifact, or unavailable current reviewer
// after a fully observed result fails the pass. Unknown delivery/result remains held.
// Callers establish external scope and serialize owner intervention as usual.
func (s *Store) ResolvePipelinePolicyReview(ctx context.Context, swarmID string, sequence int64) (PipelinePolicyTransitionResolution, error) {
	var out PipelinePolicyTransitionResolution
	err := s.WithTx(ctx, func(ctx context.Context) error {
		r, a, err := s.inlinePolicyReview(ctx, swarmID, sequence)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		w, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		v, err := s.GetSwarmStep(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		if v.State == "completed" || w.State != "active" || !activeExecution(w.Execution) {
			return nil
		}
		var latest int64
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?`, swarmID).Scan(&latest); err != nil {
			return err
		}
		if latest != sequence {
			return ErrPipelineVisitConflict
		}
		interrupted, err := s.interruptedStep(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		if interrupted {
			return ErrPipelineVisitInterrupted
		}
		reason := ""
		var intent *PipelineCompletionIntent
		switch v.State {
		case "refused":
			if _, err := s.GetPipelineCompletionIntent(ctx, swarmID, sequence); err == nil {
				return ErrPipelineVisitConflict
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
			reason = "independent review unavailable: " + v.Detail
		case "pending":
			if err := s.requireRoleActor(ctx, swarmID, r.Role, a.ActorUID, a.Incarnation); err != nil {
				reason = fmt.Sprintf("independent review unavailable: %v", err)
			}
		case "inflight", "delivering", "unconfirmed":
			// Отсутствие, timeout и unknown observation не означают отказ review.
			// Известный refused означает недоступность проверки, не actor fail.
			i, err := s.GetPipelineCompletionIntent(ctx, swarmID, sequence)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			o, err := s.GetPipelineResultObservation(ctx, swarmID, sequence)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if o.State != "observed" && o.State != "refused" {
				return nil
			}
			if err := s.requireCompletionReceipt(ctx, i); err != nil {
				return err
			}
			if w.Source == nil || i.Origin != w.Source.Origin || i.ActorUID != a.ActorUID || i.Incarnation != a.Incarnation || i.BaseCommit != r.Artifact || i.Pass != v.Pass || i.RequestID != v.RequestID || v.ActorUID != a.ActorUID || v.ActorIncarnation != a.Incarnation || v.AttestedCommit != r.Artifact || o.Intent != i {
				return ErrPipelineVisitConflict
			}
			if err := s.checkChildParentResult(context.WithValue(ctx, childJoinCompletionKey{}, true), swarmID, sequence, i.Outcome, i.Report); err != nil {
				return err
			}
			if err := s.checkResultCoordination(ctx, i); err != nil {
				return err
			}
			intent = &i
			if o.State == "refused" {
				reason = "independent review unavailable: root result observation refused: " + o.Detail
				break
			}
			e, err := s.GetPipelineResultEvidence(ctx, swarmID, sequence)
			if err != nil {
				return err
			}
			if o.Request != e.ResultRequest {
				return ErrPipelineVisitConflict
			}
			switch {
			case i.Outcome != "success":
				reason = "independent review rejected the artifact"
			case e.Head != r.Artifact || len(e.Paths) != 0:
				reason = "independent review did not preserve the assigned artifact"
			default:
				if err := s.requireRoleActor(ctx, swarmID, r.Role, a.ActorUID, a.Incarnation); err != nil {
					reason = fmt.Sprintf("independent review unavailable: %v", err)
				}
			}
		default:
			return nil
		}
		if reason == "" {
			return nil
		}
		if intent == nil {
			if err := s.guardOwnerIntervention(ctx, a.ActorUID); err != nil {
				return err
			}
			if err := s.checkChildParentResult(ctx, swarmID, sequence, "", ""); err != nil {
				return err
			}
		}
		now := nowRFC3339()
		if intent != nil {
			_, err = s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='completed',outcome=?,report=NULLIF(?,''),finished_at=?,detail=? WHERE swarm_id=? AND sequence=?`, intent.Outcome, intent.Report, now, reason, swarmID, sequence)
		} else {
			_, err = s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='refused',finished_at=?,detail=? WHERE swarm_id=? AND sequence=?`, now, reason, swarmID, sequence)
		}
		if err != nil {
			return err
		}
		if err := s.advanceExecution(ctx, swarmID, "failed", now); err != nil {
			return err
		}
		if err := s.queueTelegram(ctx, "pass_finished", TelegramEvent{Swarm: swarmID, Sequence: v.Pass, Node: v.NodeID, Execution: "failed"}); err != nil {
			return err
		}
		out = PipelinePolicyTransitionResolution{CandidateID: r.CandidateID, Applied: true, Rejected: true, Decision: workflowpolicy.Decision{State: workflowpolicy.Denied, Denied: []string{reason}}}
		return nil
	})
	return out, err
}
