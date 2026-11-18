package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/workflowpolicy"
)

// PipelinePolicyPreview is a detached evaluation of a current candidate.
// It grants no permission to apply the transition, reserve an actor, or send work.
// Callers enforce material visibility; an eventual writer must recheck in its transaction.
// Artifact identities and any future review records are scoped to Policy's exact pass.
type PipelinePolicyPreview struct {
	SwarmID    string
	Sequence   int64
	Policy     SwarmPassPolicy
	Transition pipeline.Transition
	EvidenceID string
	Input      workflowpolicy.Input
	Decision   workflowpolicy.Decision
}

// PreviewInitialPipelinePolicy evaluates only the unstarted first visit of a pass.
// Initial context is derived from the visit and absence of any completion intent
// in that pass, never from missing or refused result evidence.
func (s *Store) PreviewInitialPipelinePolicy(ctx context.Context, swarmID string, sequence int64) (PipelinePolicyPreview, error) {
	var result PipelinePolicyPreview
	err := s.WithTx(ctx, func(ctx context.Context) error {
		w, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		visit, err := s.GetSwarmStep(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		if w.State != "active" || !activeExecution(w.Execution) || visit.Pass != sequence || visit.Goal == "" || visit.State != "pending" && visit.State != "waiting" || visit.ActorUID != "" || visit.RequestID != "" {
			return ErrPipelineVisitConflict
		}
		var latest int64
		var produced bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT (SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?), EXISTS(SELECT 1 FROM pipeline_completion_intents WHERE swarm_id=? AND pass_sequence=?)`, swarmID, swarmID, visit.Pass).Scan(&latest, &produced); err != nil {
			return err
		}
		if latest != sequence || produced {
			return ErrPipelineVisitConflict
		}
		graph, err := pipeline.Parse(string(w.Graph), "pipeline")
		if err != nil {
			return err
		}
		transition, err := pipeline.Begin(graph)
		if err != nil {
			return err
		}
		if transition.Next != visit.NodeID || transition.NextState != visit.State {
			return ErrPipelineVisitConflict
		}
		preview := PipelinePolicyPreview{SwarmID: swarmID, Sequence: sequence, Transition: transition, Input: workflowpolicy.Input{Initial: true}}
		result, err = s.evaluatePipelinePolicyPreview(ctx, w, preview, graph.Nodes[transition.Next])
		return err
	})
	if err != nil {
		return PipelinePolicyPreview{}, err
	}
	return result, nil
}

// PreviewActorResultPolicy evaluates a successor or terminal no-edge result from
// the exact accepted report and retained verified artifact. It never promotes
// report prose to evidence or approvals, or creates an implicit exit visit.
func (s *Store) PreviewActorResultPolicy(ctx context.Context, swarmID string, sequence int64) (PipelinePolicyPreview, error) {
	var result PipelinePolicyPreview
	err := s.WithTx(ctx, func(ctx context.Context) error {
		intent, err := s.GetPipelineCompletionIntent(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		if err := s.checkResultIntent(ctx, intent); err != nil {
			return err
		}
		observation, err := s.GetPipelineResultObservation(ctx, swarmID, sequence)
		if errors.Is(err, ErrNotFound) || err == nil && observation.State != "observed" {
			return fmt.Errorf("%w: verified result observation is required", ErrPipelineCompletionPending)
		}
		if err != nil {
			return err
		}
		signed, err := s.GetPipelineResultEvidence(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		if signed.ResultRequest != observation.Request || observation.Intent != intent {
			return ErrPipelineVisitConflict
		}
		w, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		if w.State != "active" {
			return ErrPipelineVisitConflict
		}
		visit, err := s.GetSwarmStep(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		graph, err := pipeline.Parse(string(w.Graph), "pipeline")
		if err != nil {
			return err
		}
		transition, err := pipeline.Advance(graph, pipeline.Event{Node: visit.NodeID, State: "completed", Outcome: intent.Outcome})
		if err != nil {
			return err
		}
		evidence := workflowpolicy.Evidence{
			Known: true, Complete: true,
			Artifact: signed.Head, ProducerUID: signed.ActorUID,
			Paths: slices.Clone(signed.Paths),
		}
		evidence, err = s.policyArtifactEvidence(ctx, swarmID, sequence, evidence)
		if err != nil {
			return err
		}
		preview := PipelinePolicyPreview{
			SwarmID: swarmID, Sequence: sequence,
			Transition: transition, EvidenceID: observation.ID,
			Input: workflowpolicy.Input{Evidence: evidence},
		}
		result, err = s.evaluatePipelinePolicyPreview(ctx, w, preview, graph.Nodes[transition.Next])
		return err
	})
	if err != nil {
		return PipelinePolicyPreview{}, err
	}
	return result, nil
}

func (s *Store) evaluatePipelinePolicyPreview(ctx context.Context, w Swarm, preview PipelinePolicyPreview, node pipeline.Node) (PipelinePolicyPreview, error) {
	policy, err := s.SwarmStepWorkflowPolicy(ctx, w.ID, preview.Sequence)
	if err != nil {
		return PipelinePolicyPreview{}, err
	}
	if policy.ProjectID != w.ProjectID {
		return PipelinePolicyPreview{}, ErrPipelineVisitConflict
	}
	preview.Policy = policy
	preview.Input.TargetKind = node.Type
	if preview.Transition.Terminal {
		preview.Input.TargetKind = "exit"
	}
	if node.Type == "actor" {
		preview.Input.ActionsKnown = len(node.ActionClasses) != 0
		preview.Input.Actions = slices.Clone(node.ActionClasses)
	}
	compiled, err := workflowpolicy.New(policy.Rules)
	if err != nil {
		return PipelinePolicyPreview{}, err
	}
	preview.Decision, err = compiled.Evaluate(preview.Input)
	return preview, err
}
