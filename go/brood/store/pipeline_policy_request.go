package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"brood.dev/go/brood/workflowpolicy"
)

const policyRequestPrefix = "policy:"

func (s *Store) queuePipelinePolicyNotification(ctx context.Context, kind string, candidate PipelinePolicyCandidate) error {
	preview := candidate.Preview
	if preview.Decision.State != workflowpolicy.WaitingHuman || len(preview.Decision.Human) == 0 {
		return nil
	}
	node, err := s.stepNode(ctx, preview.SwarmID, preview.Sequence)
	if err != nil {
		return err
	}
	return s.queueTelegram(ctx, kind, TelegramEvent{Swarm: preview.SwarmID, Request: policyRequestPrefix + candidate.ID, Sequence: preview.Sequence, Node: node})
}

func (s *Store) pipelinePolicyRequest(ctx context.Context, swarmID, candidateID string) (SwarmRequest, error) {
	var result SwarmRequest
	err := s.WithTx(ctx, func(ctx context.Context) error {
		candidate, err := s.GetPipelinePolicyCandidateByID(ctx, swarmID, candidateID)
		if err != nil {
			return err
		}
		preview := candidate.Preview
		if preview.Decision.State != workflowpolicy.WaitingHuman || len(preview.Decision.Human) == 0 {
			return ErrNotFound
		}
		question := "Approve automatic transition?"
		details := "Complete matching paths are retained in the policy candidate."
		if preview.Transition.Terminal {
			question = "Finalize the reported result without a successor?"
			details = "Run ends failed because no edge matches the reported outcome. " + details
		}
		material, err := json.Marshal(struct {
			CandidateID    string   `json:"candidate_id"`
			PolicyRevision int64    `json:"policy_revision"`
			PassSequence   int64    `json:"pass_sequence"`
			Target         string   `json:"target"`
			Terminal       bool     `json:"terminal,omitempty"`
			Artifact       string   `json:"artifact"`
			HumanRules     []string `json:"human_rules"`
			Details        string   `json:"details"`
		}{CandidateID: candidate.ID, PolicyRevision: preview.Policy.Revision, PassSequence: preview.Policy.Pass,
			Target: preview.Transition.Next, Terminal: preview.Transition.Terminal, Artifact: preview.Input.Evidence.Artifact,
			HumanRules: preview.Decision.Human, Details: details})
		if err != nil {
			return err
		}
		if err := validRequestText(string(material), false); err != nil {
			return err
		}
		sequence := preview.Sequence
		result = SwarmRequest{ID: policyRequestPrefix + candidate.ID, SwarmID: swarmID, Sequence: &sequence, Origin: "policy", Kind: "confirm", Question: question, Context: string(material), State: "open", CreatedAt: candidate.CreatedAt}
		confirmation, err := s.GetPipelinePolicyConfirmation(ctx, swarmID, candidate.ID)
		if err == nil {
			result.State, result.AnsweredBy = "answered", confirmation.UserID
			result.AnsweredAt, result.Response = &confirmation.AnsweredAt, &confirmation.Answer
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		current, err := s.previewPipelinePolicyCandidate(ctx, swarmID, preview.Sequence, candidate.Boundary)
		if errors.Is(err, ErrPipelineVisitConflict) || errors.Is(err, ErrNotFound) {
			result.State = "cancelled"
			return nil
		}
		if err != nil {
			return err
		}
		if !samePolicyPreview(current, preview) {
			result.State = "cancelled"
		}
		return nil
	})
	if err != nil {
		return SwarmRequest{}, err
	}
	return result, nil
}

func (s *Store) pipelinePolicyRequests(ctx context.Context, swarmID string) ([]SwarmRequest, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT id FROM pipeline_policy_candidates WHERE swarm_id=? AND json_extract(preview,'$.Decision.State')='waiting_human' ORDER BY created_at,id`, swarmID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	result := make([]SwarmRequest, 0, len(ids))
	for _, id := range ids {
		request, err := s.pipelinePolicyRequest(ctx, swarmID, id)
		if err != nil {
			return nil, err
		}
		result = append(result, request)
	}
	return result, nil
}

func (s *Store) answerPipelinePolicyRequest(ctx context.Context, in SwarmAnswer, authorize func(context.Context) bool) (SwarmRequest, error) {
	if in.Outcome != "" {
		return SwarmRequest{}, ErrSwarmRequestInvalid
	}
	candidateID := strings.TrimPrefix(in.RequestID, policyRequestPrefix)
	_, err := s.AnswerPipelinePolicy(ctx, PipelinePolicyAnswer{SwarmID: in.SwarmID, CandidateID: candidateID, UserID: in.UserID, ExpectedRevision: in.ExpectedRevision, Answer: in.Response}, authorize)
	if err != nil {
		return SwarmRequest{}, err
	}
	return s.pipelinePolicyRequest(ctx, in.SwarmID, candidateID)
}
