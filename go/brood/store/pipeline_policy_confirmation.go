package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"brood.dev/go/brood/workflowpolicy"
)

// ErrPolicyConfirmationConflict means the candidate cannot accept this answer.
var ErrPolicyConfirmationConflict = fmt.Errorf("%w: policy confirmation conflict", ErrPipelineVisitConflict)

// PipelinePolicyAnswer addresses the implicit pending revision (1) of one candidate.
// Authorization comes from the current user/session, never from these fields alone.
type PipelinePolicyAnswer struct {
	SwarmID, CandidateID, UserID string
	ExpectedRevision             int64
	Answer                       string
}

// PipelinePolicyConfirmation is immutable answer history, not transition permission.
// Revision 2 records the first response to pending revision 1; no answer is fabricated
// for stopped candidates. Both yes and no leave graph application to the policy engine.
type PipelinePolicyConfirmation struct {
	SwarmID, CandidateID, UserID string
	Revision                     int64
	Answer, AnsweredAt           string
}

// GetPipelinePolicyConfirmation reads history scoped to a swarm. No row means no
// answer, not proof that the candidate is still open. Callers authorize visibility.
func (s *Store) GetPipelinePolicyConfirmation(ctx context.Context, swarmID, candidateID string) (PipelinePolicyConfirmation, error) {
	var result PipelinePolicyConfirmation
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT swarm_id,candidate_id,user_id,revision,answer,answered_at FROM pipeline_policy_confirmations WHERE swarm_id=? AND candidate_id=?`, swarmID, candidateID).Scan(&result.SwarmID, &result.CandidateID, &result.UserID, &result.Revision, &result.Answer, &result.AnsweredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelinePolicyConfirmation{}, ErrNotFound
	}
	return result, err
}

// AnswerPipelinePolicy records a manager's one-shot response for the exact candidate.
// The callback rechecks session/CSRF in the write transaction. Exact retries return
// historical responses without reapplying anything, even after stop. A new answer
// requires the candidate's current unchanged context and an unsatisfied human rule.
func (s *Store) AnswerPipelinePolicy(ctx context.Context, in PipelinePolicyAnswer, authorize func(context.Context) bool) (PipelinePolicyConfirmation, error) {
	if in.ExpectedRevision != 1 {
		return PipelinePolicyConfirmation{}, ErrPolicyConfirmationConflict
	}
	if in.Answer != "yes" && in.Answer != "no" {
		return PipelinePolicyConfirmation{}, fmt.Errorf("%w: policy answer must be yes or no", ErrSwarmRequestInvalid)
	}
	var result PipelinePolicyConfirmation
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.manageableSwarm(ctx, in.SwarmID, in.UserID, "answer policy", authorize); err != nil {
			return err
		}
		if err := s.refuseSwarmEval(ctx, in.SwarmID); err != nil {
			return err
		}
		candidate, err := s.GetPipelinePolicyCandidateByID(ctx, in.SwarmID, in.CandidateID)
		if err != nil {
			return err
		}
		if candidate.Preview.Decision.State != workflowpolicy.WaitingHuman || len(candidate.Preview.Decision.Human) == 0 {
			return ErrPolicyConfirmationConflict
		}
		previous, err := s.GetPipelinePolicyConfirmation(ctx, in.SwarmID, in.CandidateID)
		if err == nil {
			if previous.Answer != in.Answer || previous.UserID != in.UserID {
				return ErrPolicyConfirmationConflict
			}
			result = previous
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		preview, err := s.previewPipelinePolicyCandidate(ctx, in.SwarmID, candidate.Preview.Sequence, candidate.Boundary)
		if err != nil {
			return err
		}
		if !samePolicyPreview(preview, candidate.Preview) {
			return ErrPolicyConfirmationConflict
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_policy_confirmations(candidate_id,swarm_id,revision,user_id,answer,answered_at) VALUES(?,?,2,?,?,?)`, in.CandidateID, in.SwarmID, in.UserID, in.Answer, nowRFC3339())
		if err != nil {
			return err
		}
		result, err = s.GetPipelinePolicyConfirmation(ctx, in.SwarmID, in.CandidateID)
		if err != nil {
			return err
		}
		return s.queuePipelinePolicyNotification(ctx, "request_answered", candidate)
	})
	if err != nil {
		return PipelinePolicyConfirmation{}, err
	}
	return result, nil
}
