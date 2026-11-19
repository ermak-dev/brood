package store

import (
	"context"
	"errors"
	"strconv"

	"brood.dev/go/brood/proxmox/attest"
)

// SwarmPassTrace is a consistent read of retained pass history, not replay
// authority. CurrentExecution is absent for historical passes whose execution
// state is not independently retained. Unscoped actor questions are explicitly
// separate from the pass rather than attributed through timestamps.
type SwarmPassTrace struct {
	Version                            int
	SwarmID, ProjectID                 string
	Pass                               int64
	Goal, SourceCommit, Origin         string
	Graph                              string
	DefinitionName, DefinitionRevision string
	DefinitionVersion                  int64
	Current                            bool
	CurrentExecution                   string
	Steps                              []SwarmPassTraceStep
	Requests                           []SwarmPassTraceRequest
	UnscopedActorRequests              []SwarmRequest
	Gaps                               []SwarmPassTraceGap
}

// SwarmPassTraceStep keeps reported work and policy evidence separate from the
// visit's actual outcome. Missing legacy instruction payloads stay absent.
type SwarmPassTraceStep struct {
	Visit             PipelineVisit
	Children          *SwarmChildJoin
	EphemeralJobs     []EphemeralChildHistory
	Completion        *PipelineCompletionIntent
	ResultObservation *PipelineResultObservation
	ResultEvidence    *attest.ResultAttestation
	PolicyCandidates  []PipelinePolicyCandidate
	Instruction       *SwarmPassTraceDelivery
	ChildInstructions []SwarmPassTraceDelivery
}

// SwarmPassTraceRequest preserves the independent actor-question contract.
// CreatedDuring does not make the question belong to a graph visit.
type SwarmPassTraceRequest struct {
	Request       SwarmRequest
	CreatedDuring *SwarmRequestPassContext
	Delivery      *SwarmPassTraceDelivery
}

// SwarmPassTraceDelivery contains the original payload, saved native attempts,
// and optional supervisor inbox evidence. Neither is authority to deliver.
type SwarmPassTraceDelivery struct {
	Intent          ActorDeliveryIntent
	Attempts        []ActorDeliveryObservation
	SupervisorInbox *ActorDeliveryInboxState
}

// SwarmPassTraceGap names unavailable history without reconstructing an effect.
type SwarmPassTraceGap struct {
	Kind      string
	Sequence  int64
	RequestID string
}

// GetSwarmPassTrace reads only the pass whose first visit is pass. It does not
// advance execution, reconcile receipts, join children or observe a workspace.
// Trusted human adapters must enforce current swarm visibility before exposing
// this material and must project public fields instead of serializing root RPC
// leases or acceptance credentials directly.
func (s *Store) GetSwarmPassTrace(ctx context.Context, swarmID string, pass int64) (SwarmPassTrace, error) {
	if !validPipelineField(swarmID) || pass < 1 {
		return SwarmPassTrace{}, ErrNotFound
	}
	var result SwarmPassTrace
	err := s.WithTx(ctx, func(ctx context.Context) error {
		swarm, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		first, err := s.GetSwarmStep(ctx, swarmID, pass)
		if err != nil {
			return err
		}
		if first.Pass != pass || first.Goal == "" {
			return ErrNotFound
		}
		var next any
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT MIN(sequence) FROM swarm_steps WHERE swarm_id=? AND goal IS NOT NULL AND sequence>?`, swarmID, pass).Scan(&next); err != nil {
			return err
		}
		result = SwarmPassTrace{Version: 1, SwarmID: swarmID, ProjectID: swarm.ProjectID, Pass: pass,
			Goal: first.Goal, SourceCommit: first.SourceCommit, Graph: string(swarm.Graph),
			DefinitionName: swarm.DefinitionName, DefinitionVersion: swarm.DefinitionVersion, DefinitionRevision: swarm.DefinitionRevision,
			Current: next == nil, Steps: []SwarmPassTraceStep{}, Requests: []SwarmPassTraceRequest{}, UnscopedActorRequests: []SwarmRequest{}, Gaps: []SwarmPassTraceGap{}}
		if result.Current {
			result.CurrentExecution = swarm.Execution
		}
		if swarm.Source != nil {
			result.Origin = swarm.Source.Origin
		}
		budget := &passTraceBudget{}
		if err := budget.add(result); err != nil {
			return err
		}
		visits, err := s.passTraceVisits(ctx, swarmID, pass, next, budget)
		if err != nil {
			return err
		}
		sequences := map[int64]bool{}
		for _, visit := range visits {
			item, err := s.passTraceStep(ctx, swarmID, visit)
			if err != nil {
				return err
			}
			extra := item
			extra.Visit = PipelineVisit{}
			if err := budget.add(extra); err != nil {
				return err
			}
			result.Steps = append(result.Steps, item)
			sequences[visit.Sequence] = true
			if visit.ActorUID != "" && visit.RequestID != "" && item.Instruction == nil {
				gap := SwarmPassTraceGap{Kind: "instruction_not_retained", Sequence: visit.Sequence}
				if err := budget.add(gap); err != nil {
					return err
				}
				result.Gaps = append(result.Gaps, gap)
			}
		}
		return s.passTraceRequests(ctx, &result, sequences, budget)
	})
	if err != nil {
		return SwarmPassTrace{}, err
	}
	return result, nil
}

func (s *Store) passTraceVisits(ctx context.Context, swarmID string, pass int64, next any, budget *passTraceBudget) ([]PipelineVisit, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, swarmStepSelect+` WHERE s.swarm_id=? AND s.sequence>=? AND (? IS NULL OR s.sequence<?) ORDER BY s.sequence LIMIT ?`, swarmID, pass, next, next, MaxPassTraceRecords+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PipelineVisit{}
	for rows.Next() {
		visit, err := scanSwarmStep(rows)
		if err != nil {
			return nil, err
		}
		visit.Pass = pass
		if err := budget.record(visit); err != nil {
			return nil, err
		}
		result = append(result, visit)
	}
	return result, rows.Err()
}

func (s *Store) passTraceStep(ctx context.Context, swarmID string, visit PipelineVisit) (SwarmPassTraceStep, error) {
	result := SwarmPassTraceStep{Visit: visit, PolicyCandidates: []PipelinePolicyCandidate{}}
	jobs, err := s.EphemeralChildHistoryAtStep(ctx, swarmID, visit.Sequence)
	if err != nil {
		return result, err
	}
	result.EphemeralJobs = jobs
	children, err := s.GetSwarmChildHistory(ctx, swarmID, visit.Sequence)
	if err == nil {
		result.Children = &children
		for _, child := range children.Children {
			instruction, err := s.passTraceDelivery(ctx, swarmID, "child", child.ID)
			if err != nil {
				return result, err
			}
			if instruction != nil {
				result.ChildInstructions = append(result.ChildInstructions, *instruction)
			}
		}
	} else if !errors.Is(err, ErrNotFound) {
		return result, err
	}
	completion, err := s.GetPipelineCompletionIntent(ctx, swarmID, visit.Sequence)
	if err == nil {
		result.Completion = &completion
	} else if !errors.Is(err, ErrNotFound) {
		return result, err
	}
	observation, err := s.GetPipelineResultObservation(ctx, swarmID, visit.Sequence)
	if err == nil {
		result.ResultObservation = &observation
	} else if !errors.Is(err, ErrNotFound) {
		return result, err
	}
	evidence, err := s.GetPipelineResultEvidence(ctx, swarmID, visit.Sequence)
	if err == nil {
		result.ResultEvidence = &evidence
	} else if !errors.Is(err, ErrNotFound) {
		return result, err
	}
	for _, boundary := range []string{"entry", "result"} {
		candidate, err := s.GetPipelinePolicyCandidate(ctx, swarmID, visit.Sequence, boundary)
		if err == nil {
			result.PolicyCandidates = append(result.PolicyCandidates, candidate)
		} else if !errors.Is(err, ErrNotFound) {
			return result, err
		}
	}
	result.Instruction, err = s.passTraceDelivery(ctx, swarmID, "step", strconv.FormatInt(visit.Sequence, 10))
	return result, err
}

func (s *Store) passTraceDelivery(ctx context.Context, swarmID, kind, source string) (*SwarmPassTraceDelivery, error) {
	intent, err := s.GetActorDeliveryForSource(ctx, swarmID, kind, source)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	attempts, err := s.GetActorDeliveryHistory(ctx, intent.ID)
	if err != nil {
		return nil, err
	}
	result := &SwarmPassTraceDelivery{Intent: intent, Attempts: attempts}
	inbox, err := s.ActorDeliveryInboxStatus(ctx, intent.ID)
	if err == nil {
		result.SupervisorInbox = &inbox
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return result, nil
}
