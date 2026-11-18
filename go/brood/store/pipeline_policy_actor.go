package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"brood.dev/go/brood/pipeline"
	"brood.dev/go/brood/workflowpolicy"
)

// ErrNoPolicyActor means this result does not target an actor graph node.
var ErrNoPolicyActor = errors.New("policy result does not target an actor node")

// ResolvePipelinePolicyActor applies an exact verified result to its actor successor.
// It creates no lease or guest effect. Callers must establish current external actor
// scope first; reservation and transport boundaries recheck the durable permission.
func (s *Store) ResolvePipelinePolicyActor(ctx context.Context, swarmID string, sequence int64) (PipelinePolicyTransitionResolution, error) {
	return s.resolvePipelinePolicyTransition(ctx, swarmID, sequence, "actor", ErrNoPolicyActor)
}

// PreviewPipelineActorEntryPolicy describes an initial or post-human pending actor
// entry. Immediate actor successors instead use their predecessor's result candidate.
// A graph answer supplies routing, never approval.
func (s *Store) PreviewPipelineActorEntryPolicy(ctx context.Context, swarmID string, sequence int64) (PipelinePolicyPreview, error) {
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
		var latest int64
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?`, swarmID).Scan(&latest); err != nil {
			return err
		}
		if w.State != "active" || !activeExecution(w.Execution) || latest != sequence || visit.State != "pending" || visit.ActorUID != "" || visit.RequestID != "" {
			return ErrPipelineVisitConflict
		}
		graph, err := pipeline.Parse(string(w.Graph), "pipeline")
		if err != nil {
			return err
		}
		var boundary string
		result, boundary, err = s.actorEntryPolicyPreview(ctx, w, graph, sequence)
		if err == nil && boundary != "entry" {
			return ErrPipelineVisitConflict
		}
		return err
	})
	if err != nil {
		return PipelinePolicyPreview{}, err
	}
	return result, nil
}

// actorEntryPolicyPreview also runs after reservation at each final effect fence.
// Immediate actor successors reuse the exact result candidate and its answers;
// after human routing a separate entry candidate binds the new target to the last
// verified artifact in this pass. Missing result evidence never becomes Initial.
func (s *Store) actorEntryPolicyPreview(ctx context.Context, w Swarm, graph *pipeline.Document, sequence int64) (PipelinePolicyPreview, string, error) {
	visit, err := s.GetSwarmStep(ctx, w.ID, sequence)
	if err != nil {
		return PipelinePolicyPreview{}, "", err
	}
	if visit.Pass == 0 || graph.Nodes[visit.NodeID].Type != "actor" {
		return PipelinePolicyPreview{}, "", ErrPipelineVisitConflict
	}
	preview := PipelinePolicyPreview{SwarmID: w.ID, Sequence: sequence, Transition: pipeline.Transition{Next: visit.NodeID, NextState: "pending", Execution: "running"}, Input: workflowpolicy.Input{Initial: true}}
	expectedNode := visit.NodeID
	boundary := "entry"
	for previous := sequence - 1; previous >= visit.Pass; previous-- {
		source, err := s.GetSwarmStep(ctx, w.ID, previous)
		if err != nil {
			return PipelinePolicyPreview{}, "", err
		}
		interrupted, err := s.interruptedStep(ctx, w.ID, previous)
		if err != nil {
			return PipelinePolicyPreview{}, "", err
		}
		if interrupted {
			return PipelinePolicyPreview{}, "", ErrPipelineVisitInterrupted
		}
		node := graph.Nodes[source.NodeID]
		var event pipeline.Event
		switch node.Type {
		case "human":
			var outcome string
			if source.State != "answered" {
				return PipelinePolicyPreview{}, "", ErrPipelineVisitConflict
			}
			if err := s.conn(ctx).QueryRowContext(ctx, `SELECT outcome FROM swarm_requests WHERE swarm_id=? AND sequence=? AND origin='node' AND state='answered'`, w.ID, previous).Scan(&outcome); err != nil {
				return PipelinePolicyPreview{}, "", err
			}
			event = pipeline.Event{Node: source.NodeID, State: "answered", Outcome: outcome}
		case "actor":
			if source.State != "completed" || source.Outcome == nil {
				return PipelinePolicyPreview{}, "", ErrPipelineVisitConflict
			}
			intent, err := s.GetPipelineCompletionIntent(ctx, w.ID, previous)
			if err != nil {
				return PipelinePolicyPreview{}, "", err
			}
			if intent.Pass != visit.Pass || intent.RequestID != source.RequestID || intent.ActorUID != source.ActorUID || intent.Incarnation != source.ActorIncarnation || intent.Outcome != *source.Outcome || intent.Report != source.Report || w.Source == nil || intent.Origin != w.Source.Origin || source.AttestedCommit != intent.BaseCommit {
				return PipelinePolicyPreview{}, "", ErrPipelineVisitConflict
			}
			if err := s.requireCompletionReceipt(ctx, intent); err != nil {
				return PipelinePolicyPreview{}, "", err
			}
			if err := s.requireRoleActor(ctx, w.ID, node.Role, source.ActorUID, source.ActorIncarnation); err != nil {
				if _, _, reviewErr := s.inlinePolicyReview(ctx, w.ID, previous); reviewErr == nil {
					return PipelinePolicyPreview{}, "", fmt.Errorf("%w: %v", errPolicyReviewUnavailable, err)
				} else if !errors.Is(reviewErr, ErrNotFound) {
					return PipelinePolicyPreview{}, "", reviewErr
				}
				return PipelinePolicyPreview{}, "", err
			}
			if err := s.guardOwnerIntervention(ctx, source.ActorUID); err != nil {
				return PipelinePolicyPreview{}, "", err
			}
			observation, err := s.GetPipelineResultObservation(ctx, w.ID, previous)
			if err != nil {
				return PipelinePolicyPreview{}, "", err
			}
			signed, err := s.GetPipelineResultEvidence(ctx, w.ID, previous)
			if err != nil {
				return PipelinePolicyPreview{}, "", err
			}
			if observation.State != "observed" || observation.Intent != intent || observation.Request != signed.ResultRequest {
				return PipelinePolicyPreview{}, "", ErrPipelineVisitConflict
			}
			preview.Input = workflowpolicy.Input{Evidence: workflowpolicy.Evidence{Known: true, Complete: true, Artifact: signed.Head, ProducerUID: signed.ActorUID, Paths: slices.Clone(signed.Paths)}}
			preview.Input.Evidence, err = s.policyArtifactEvidence(ctx, w.ID, previous, preview.Input.Evidence)
			if err != nil {
				return PipelinePolicyPreview{}, "", err
			}
			preview.EvidenceID = observation.ID
			event = pipeline.Event{Node: source.NodeID, State: "completed", Outcome: *source.Outcome}
		default:
			return PipelinePolicyPreview{}, "", ErrPipelineVisitConflict
		}
		transition, err := pipeline.Advance(graph, event)
		if err != nil {
			return PipelinePolicyPreview{}, "", err
		}
		if transition.Terminal || transition.Next != expectedNode {
			return PipelinePolicyPreview{}, "", ErrPipelineVisitConflict
		}
		if node.Type == "actor" {
			if previous == sequence-1 {
				preview.Sequence = previous
				preview.Transition = transition
				boundary = "result"
			}
			result, err := s.evaluatePipelinePolicyPreview(ctx, w, preview, graph.Nodes[visit.NodeID])
			return result, boundary, err
		}
		expectedNode = source.NodeID
	}
	begin, err := pipeline.Begin(graph)
	if err != nil {
		return PipelinePolicyPreview{}, "", err
	}
	if begin.Next != expectedNode {
		return PipelinePolicyPreview{}, "", ErrPipelineVisitConflict
	}
	// Even a pass with no prior actor cannot ignore an accepted-but-unobserved result.
	var produced bool
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pipeline_completion_intents WHERE swarm_id=? AND pass_sequence=?)`, w.ID, visit.Pass).Scan(&produced); err != nil {
		return PipelinePolicyPreview{}, "", err
	}
	if produced {
		return PipelinePolicyPreview{}, "", ErrPipelineVisitConflict
	}
	result, err := s.evaluatePipelinePolicyPreview(ctx, w, preview, graph.Nodes[visit.NodeID])
	return result, boundary, err
}
