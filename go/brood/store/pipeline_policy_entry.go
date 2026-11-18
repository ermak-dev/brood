package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/workflowpolicy"
)

// ErrPipelinePolicyPending means no automatic actor effect is permitted yet.
var ErrPipelinePolicyPending = fmt.Errorf("%w: automatic entry awaits policy resolution", ErrPipelineVisitConflict)

func (s *Store) actorEntryPolicyAllowed(ctx context.Context, w Swarm, graph *pipeline.Document, sequence int64) (bool, error) {
	policy, err := s.SwarmStepWorkflowPolicy(ctx, w.ID, sequence)
	if err != nil {
		return false, err
	}
	if len(policy.Rules) == 0 {
		return true, nil
	}
	interrupted, err := s.interruptedStep(ctx, w.ID, sequence)
	if err != nil {
		return false, err
	}
	if interrupted {
		return false, ErrPipelineVisitInterrupted
	}
	expected, boundary, err := s.actorEntryPolicyPreview(ctx, w, graph, sequence)
	if err != nil {
		return false, err
	}
	candidate, err := s.GetPipelinePolicyCandidate(ctx, w.ID, expected.Sequence, boundary)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !samePolicyPreview(candidate.Preview, expected) {
		return false, ErrPipelineVisitConflict
	}
	decision, rejected, err := s.pipelinePolicyCandidateDecision(ctx, candidate)
	return !rejected && (decision.State == workflowpolicy.Allowed || s.pipelinePolicyReviewEntryAllowed(ctx, candidate, decision)), err
}

func policyRejectionReason(decision workflowpolicy.Decision) string {
	if len(decision.Denied) > 0 {
		return "automatic transition denied by workflow policy rules: " + strings.Join(decision.Denied, ", ")
	}
	return "automatic transition rejected by workflow policy confirmation"
}

func (s *Store) pipelinePolicyCandidateDecision(ctx context.Context, candidate PipelinePolicyCandidate) (workflowpolicy.Decision, bool, error) {
	input := candidate.Preview.Input
	confirmation, err := s.GetPipelinePolicyConfirmation(ctx, candidate.Preview.SwarmID, candidate.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return workflowpolicy.Decision{}, false, err
	}
	rejected := err == nil && confirmation.Answer == "no"
	if err == nil && confirmation.Answer == "yes" {
		input.Approved = map[string]bool{}
		for _, rule := range candidate.Preview.Decision.Human {
			input.Approved[rule] = true
		}
	}
	compiled, err := workflowpolicy.New(candidate.Preview.Policy.Rules)
	if err != nil {
		return workflowpolicy.Decision{}, false, err
	}
	decision, err := compiled.Evaluate(input)
	if err != nil || rejected || decision.State == workflowpolicy.Denied {
		return decision, rejected || decision.State == workflowpolicy.Denied, err
	}
	if decision.State == workflowpolicy.WaitingHuman || decision.State == workflowpolicy.Blocked {
		return decision, false, nil
	}
	reason, err := s.policyReviewFailure(ctx, candidate, workflowpolicy.Decision{State: decision.State})
	if err != nil {
		return workflowpolicy.Decision{}, false, err
	}
	if reason != "" {
		decision.State = workflowpolicy.Denied
		decision.Denied = append(decision.Denied, reason)
		return decision, true, nil
	}
	input.Reviews, err = s.pipelinePolicyReviews(ctx, candidate)
	if errors.Is(err, errPolicyReviewUnavailable) {
		decision.State = workflowpolicy.Denied
		decision.Denied = append(decision.Denied, err.Error())
		return decision, true, nil
	}
	if err != nil {
		return workflowpolicy.Decision{}, false, err
	}
	decision, err = compiled.Evaluate(input)
	if err != nil {
		return decision, false, err
	}
	reason, err = s.policyReviewFailure(ctx, candidate, decision)
	if reason != "" {
		decision.State = workflowpolicy.Denied
		decision.Denied = append(decision.Denied, reason)
		return decision, true, err
	}
	return decision, false, err
}
