package store

import (
	"context"
	"errors"
	"fmt"

	"brood.dev/go/brood/pipeline"
)

// ActorCompletion identifies one reported outcome of an exact authenticated attempt.
// It is an actor report, not evidence of the result artifact or transition permission.
type ActorCompletion struct {
	SwarmID     string
	Sequence    int64
	RequestID   string
	ActorUID    string
	Incarnation int64
	Outcome     string
	Report      string
}

type checkedActorCompletion struct {
	swarm  Swarm
	visit  PipelineVisit
	graph  *pipeline.Document
	replay bool
}

func validActorCompletion(in ActorCompletion) error {
	if !validPipelineField(in.SwarmID) || in.Sequence < 1 || !validPipelineField(in.RequestID) || !validPipelineField(in.ActorUID) || in.Incarnation < 1 || in.Outcome != "success" && in.Outcome != "fail" {
		return errors.New("invalid pipeline actor completion")
	}
	return validStepReport(in.Report)
}

// PipelineCompletionUsesPolicy reads immutable pass configuration, never project defaults.
// A started pass without its required snapshot fails closed; pre-pass legacy visits
// preserve their existing completion path.
func (s *Store) PipelineCompletionUsesPolicy(ctx context.Context, swarmID string, sequence int64) (bool, error) {
	policy, err := s.SwarmStepWorkflowPolicy(ctx, swarmID, sequence)
	if errors.Is(err, ErrNotFound) {
		step, stepErr := s.GetSwarmStep(ctx, swarmID, sequence)
		if stepErr != nil || step.Pass != 0 {
			return false, ErrPipelineVisitConflict
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(policy.Rules) != 0, nil
}

func (s *Store) checkActorCompletion(ctx context.Context, in ActorCompletion) (checkedActorCompletion, error) {
	w, err := s.GetSwarm(ctx, in.SwarmID)
	if err != nil {
		return checkedActorCompletion{}, err
	}
	visit, err := s.GetSwarmStep(ctx, in.SwarmID, in.Sequence)
	if errors.Is(err, ErrNotFound) {
		return checkedActorCompletion{}, ErrPipelineVisitConflict
	}
	if err != nil {
		return checkedActorCompletion{}, err
	}
	fail := func(err error) (checkedActorCompletion, error) { return checkedActorCompletion{}, err }
	if visit.RequestID != in.RequestID || visit.ActorUID != in.ActorUID || visit.ActorIncarnation != in.Incarnation {
		return fail(ErrPipelineVisitConflict)
	}
	interrupted, err := s.interruptedStep(ctx, in.SwarmID, in.Sequence)
	if err != nil {
		return fail(err)
	}
	if interrupted {
		return fail(ErrPipelineVisitInterrupted)
	}
	if err := s.checkChildParentResult(ctx, in.SwarmID, in.Sequence, in.Outcome, in.Report); err != nil {
		return fail(err)
	}
	if w.Source != nil {
		source, err := s.PipelineActorStepSource(ctx, in.SwarmID, in.Sequence)
		commit := source.Commit
		if err != nil {
			return fail(err)
		}
		if commit == "" || visit.AttestedCommit != commit {
			return fail(ErrPipelineSourceUnverified)
		}
	}
	checked := checkedActorCompletion{swarm: w, visit: visit}
	if visit.State == "completed" && visit.Outcome != nil && *visit.Outcome == in.Outcome && visit.Report == in.Report {
		checked.replay = true
		return checked, nil
	}
	// Request ID попадает к актору только с инструкцией: точный ответ доказывает
	// доставку после attestation, даже если квитанция потеряна.
	lostReceipt := visit.State == "unconfirmed" && visit.AttestedCommit != ""
	if visit.State != "inflight" && visit.State != "delivering" && !lostReceipt || !activeExecution(w.Execution) {
		return fail(ErrPipelineVisitConflict)
	}
	graph, err := pipeline.Parse(string(w.Graph), "pipeline")
	if err != nil {
		return fail(err)
	}
	node, ok := graph.Nodes[visit.NodeID]
	if !ok || node.Type != "actor" {
		return fail(ErrPipelineVisitConflict)
	}
	if err := s.requireRoleActor(ctx, in.SwarmID, node.Role, in.ActorUID, in.Incarnation); err != nil {
		return fail(fmt.Errorf("%w: role %q: %w", ErrPipelineVisitConflict, node.Role, err))
	}
	var latest int64
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?`, in.SwarmID).Scan(&latest); err != nil {
		return fail(err)
	}
	if latest != in.Sequence {
		return fail(ErrPipelineVisitConflict)
	}
	checked.graph = graph
	return checked, nil
}
