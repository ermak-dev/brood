package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const maxPolicyCandidateBytes = 8 << 20

// PipelinePolicyCandidate retains one immutable initial evaluation.
// It is history, including when Decision is allowed, never an application grant.
// Callers authorize material visibility before reading it.
type PipelinePolicyCandidate struct {
	ID, Boundary, CreatedAt string
	Version                 int
	Preview                 PipelinePolicyPreview
}

// GetPipelinePolicyCandidate reads retained history without reinterpreting it
// against the current default, execution state, or evaluator behavior.
func (s *Store) GetPipelinePolicyCandidate(ctx context.Context, swarmID string, sequence int64, boundary string) (PipelinePolicyCandidate, error) {
	var result PipelinePolicyCandidate
	var raw string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT id,version,boundary,created_at,preview FROM pipeline_policy_candidates WHERE swarm_id=? AND sequence=? AND boundary=?`, swarmID, sequence, boundary).Scan(&result.ID, &result.Version, &result.Boundary, &result.CreatedAt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelinePolicyCandidate{}, ErrNotFound
	}
	if err != nil {
		return PipelinePolicyCandidate{}, err
	}
	if result.Version != 1 || len(raw) > maxPolicyCandidateBytes || json.Unmarshal([]byte(raw), &result.Preview) != nil || result.Preview.SwarmID != swarmID || result.Preview.Sequence != sequence {
		return PipelinePolicyCandidate{}, errors.New("invalid stored policy candidate")
	}
	return result, nil
}

// RecordPipelinePolicyCandidate derives a current candidate and saves it once.
// No caller-selected evidence, decisions, approval flags, or graph targets are accepted.
// A retry while the boundary is current retains its first identity and timestamp.
// No actor outcome, successor, request, lease, or application permission is written.
func (s *Store) RecordPipelinePolicyCandidate(ctx context.Context, swarmID string, sequence int64, boundary, id string) (PipelinePolicyCandidate, error) {
	if !validPipelineField(id) || len(id) > 128 {
		return PipelinePolicyCandidate{}, errors.New("invalid policy candidate ID")
	}
	var result PipelinePolicyCandidate
	err := s.WithTx(ctx, func(ctx context.Context) error {
		preview, err := s.previewPipelinePolicyCandidate(ctx, swarmID, sequence, boundary)
		if err != nil {
			return err
		}
		if len(preview.Policy.Rules) == 0 {
			return errors.New("empty workflow policy has no candidate decision to retain")
		}
		current, err := s.GetPipelinePolicyCandidate(ctx, swarmID, sequence, boundary)
		if err == nil {
			if !samePolicyPreview(current.Preview, preview) {
				return ErrPipelineVisitConflict
			}
			result = current
			return s.retainPipelinePolicyReviewRequirements(ctx, result)
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		raw, err := json.Marshal(preview)
		if err != nil || len(raw) > maxPolicyCandidateBytes {
			return errors.New("policy candidate exceeds storage limit")
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_policy_candidates(id,version,swarm_id,sequence,pass_sequence,boundary,policy_revision,evidence_id,preview,created_at) VALUES(?,1,?,?,?,?,?,NULLIF(?,''),?,?)`, id, swarmID, sequence, preview.Policy.Pass, boundary, preview.Policy.Revision, preview.EvidenceID, string(raw), nowRFC3339())
		if err != nil {
			return err
		}
		result, err = s.GetPipelinePolicyCandidate(ctx, swarmID, sequence, boundary)
		if err != nil {
			return err
		}
		if err := s.retainPipelinePolicyReviewRequirements(ctx, result); err != nil {
			return err
		}
		return s.queuePipelinePolicyNotification(ctx, "request_opened", result)
	})
	if err != nil {
		return PipelinePolicyCandidate{}, err
	}
	return result, nil
}

func (s *Store) previewPipelinePolicyCandidate(ctx context.Context, swarmID string, sequence int64, boundary string) (PipelinePolicyPreview, error) {
	switch boundary {
	case "entry":
		visit, err := s.GetSwarmStep(ctx, swarmID, sequence)
		if err != nil {
			return PipelinePolicyPreview{}, err
		}
		if visit.Pass == sequence {
			return s.PreviewInitialPipelinePolicy(ctx, swarmID, sequence)
		}
		return s.PreviewPipelineActorEntryPolicy(ctx, swarmID, sequence)
	case "result":
		return s.PreviewActorResultPolicy(ctx, swarmID, sequence)
	default:
		return PipelinePolicyPreview{}, errors.New("invalid policy candidate boundary")
	}
}

// GetPipelinePolicyCandidateByID reads history only within the named swarm.
func (s *Store) GetPipelinePolicyCandidateByID(ctx context.Context, swarmID, candidateID string) (PipelinePolicyCandidate, error) {
	var sequence int64
	var boundary string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,boundary FROM pipeline_policy_candidates WHERE swarm_id=? AND id=?`, swarmID, candidateID).Scan(&sequence, &boundary)
	if errors.Is(err, sql.ErrNoRows) {
		return PipelinePolicyCandidate{}, ErrNotFound
	}
	if err != nil {
		return PipelinePolicyCandidate{}, err
	}
	return s.GetPipelinePolicyCandidate(ctx, swarmID, sequence, boundary)
}

func samePolicyPreview(a, b PipelinePolicyPreview) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && string(left) == string(right)
}
