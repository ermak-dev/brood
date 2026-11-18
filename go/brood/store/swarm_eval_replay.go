package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"brood.dev/go/brood/pipeline"
)

// SwarmEvalReplayInput selects a current retained request, never an answer.
// Node identity and occurrence are derived from the candidate's saved visits.
type SwarmEvalReplayInput struct {
	CandidateSwarmID, RequestID, LauncherID string
}

// SwarmEvalReplay is an immutable local decision. A stopped decision is a
// successful durable refusal, not an applied answer; Reason explains why.
// Answer retains the actual reference request ID and original RecordedBy.
type SwarmEvalReplay struct {
	SwarmEvalReplayInput
	State, Reason, CreatedAt string
	InterruptedChildren      bool
	Answer                   *SwarmReplayAnswer
}

// GetSwarmEvalReplay reads history, not permission to answer or resume a pass.
func (s *Store) GetSwarmEvalReplay(ctx context.Context, swarmID, requestID string) (SwarmEvalReplay, error) {
	var raw string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT decision FROM swarm_eval_replays WHERE candidate_swarm_id=? AND request_id=?`, swarmID, requestID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return SwarmEvalReplay{}, ErrNotFound
	}
	if err != nil {
		return SwarmEvalReplay{}, err
	}
	var result SwarmEvalReplay
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return SwarmEvalReplay{}, err
	}
	if result.CandidateSwarmID != swarmID || result.RequestID != requestID {
		return SwarmEvalReplay{}, ErrSwarmEvalConflict
	}
	return result, nil
}

// ApplySwarmEvalReplay durably applies one exact retained graph choice, or stops
// with a retained reason for mismatch, policy approval or independent actor
// question. It never creates a human session or claims reference-human authority.
// authorize performs current local reads in the FULL transaction, including on
// retries. authorizeOwnerStop is separately required ONLY if stopping interrupts
// outstanding children; action interrupt_eval_children must positively authorize
// the launcher's current owner control, including exact retries of that decision.
// Store also checks owner/admin itself.
// A stopped decision returns nil error: callers must inspect State and Reason.
func (s *Store) ApplySwarmEvalReplay(ctx context.Context, in SwarmEvalReplayInput, authorize, authorizeOwnerStop SwarmEvalAuthorize) (SwarmEvalReplay, error) {
	var result SwarmEvalReplay
	err := s.WithDurableTx(ctx, func(tx context.Context) error {
		if authorize == nil {
			return ErrSwarmRequestDenied
		}
		b, err := s.GetSwarmEvalBinding(tx, in.CandidateSwarmID)
		if err != nil {
			return err
		}
		if in.LauncherID != b.LauncherID {
			return ErrSwarmRequestDenied
		}
		if err := s.authorizeEval(tx, "replay_eval", b, authorize); err != nil {
			return err
		}
		if !validPipelineField(in.RequestID) {
			return ErrNotFound
		}
		old, err := s.GetSwarmEvalReplay(tx, in.CandidateSwarmID, in.RequestID)
		if err == nil {
			if old.SwarmEvalReplayInput != in {
				return ErrSwarmEvalConflict
			}
			if old.InterruptedChildren {
				if err := s.authorizeEvalOwnerStop(tx, b, authorizeOwnerStop); err != nil {
					return err
				}
			}
			result = old
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		w, err := s.GetSwarm(tx, in.CandidateSwarmID)
		if err != nil {
			return err
		}
		if !activeExecution(w.Execution) || string(w.Graph) != b.Graph || w.DefinitionName != b.Definition || w.DefinitionVersion != b.DefinitionVersion || w.DefinitionRevision != b.DefinitionRevision {
			return ErrSwarmEvalConflict
		}
		req, err := s.GetSwarmRequest(tx, w.ID, in.RequestID)
		if err != nil {
			return err
		}
		if req.State != "open" {
			return ErrSwarmEvalConflict
		}
		first, err := s.GetSwarmStep(tx, w.ID, 1)
		if err != nil {
			return err
		}
		if first.Goal != b.Goal || first.SourceCommit != b.SourceCommit {
			return ErrSwarmEvalConflict
		}
		result = SwarmEvalReplay{SwarmEvalReplayInput: in, CreatedAt: nowRFC3339()}
		var answer SwarmReplayAnswer
		mismatch := replayMismatch("policy confirmations and actor questions cannot use recorded human answers")
		if req.Origin == "node" {
			if req.Sequence == nil || w.Execution != "waiting" {
				return ErrSwarmEvalConflict
			}
			var node, state string
			var latest int64
			if err := s.conn(tx).QueryRowContext(tx, `SELECT node_id,state,(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?) FROM swarm_steps WHERE swarm_id=? AND sequence=?`, w.ID, w.ID, *req.Sequence).Scan(&node, &state, &latest); err != nil {
				return err
			}
			if state != "waiting" || latest != *req.Sequence {
				return ErrSwarmEvalConflict
			}
			var occurrence int
			if err := s.conn(tx).QueryRowContext(tx, `SELECT COUNT(*) FROM swarm_steps WHERE swarm_id=? AND node_id=? AND sequence<=?`, w.ID, node, *req.Sequence).Scan(&occurrence); err != nil {
				return err
			}
			reference, err := s.GetSwarmPassTrace(tx, b.ReferenceSwarmID, b.ReferencePass)
			if err != nil {
				return err
			}
			answer, mismatch = MatchSwarmReplayAnswer(reference, node, occurrence, req)
		}
		if mismatch != nil {
			children, err := s.hasUnfinishedSwarmChildren(tx, w.ID)
			if err != nil {
				return err
			}
			if children {
				if err := s.authorizeEvalOwnerStop(tx, b, authorizeOwnerStop); err != nil {
					return err
				}
				result.InterruptedChildren = true
			}
			if _, err := s.StopSwarm(tx, w.ID, b.LauncherID, func(context.Context) bool { return true }); err != nil {
				return err
			}
			stopped, err := s.GetSwarm(tx, w.ID)
			if err != nil {
				return err
			}
			if stopped.Execution != "stopped" {
				return ErrSwarmEvalConflict
			}
			var unsafe bool
			if err := s.conn(tx).QueryRowContext(tx, `SELECT EXISTS(SELECT 1 FROM swarm_steps WHERE swarm_id=? AND state IN ('waiting','pending','attesting','delivering')) OR EXISTS(SELECT 1 FROM swarm_requests WHERE swarm_id=? AND origin='node' AND state='open') OR EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=? AND `+swarmChildUnfinished+` AND (NOT EXISTS(SELECT 1 FROM swarm_steps st WHERE st.swarm_id=j.swarm_id AND st.sequence=j.parent_sequence AND st.state='unconfirmed') OR NOT EXISTS(SELECT 1 FROM actor_step_interruptions si JOIN actor_interventions ai ON ai.id=si.intervention_id WHERE si.swarm_id=j.swarm_id AND si.sequence=j.parent_sequence AND ai.actor_uid=j.parent_uid AND ai.incarnation=j.parent_incarnation AND ai.human_id=? AND ai.operation='pass_stop' AND ai.state='completed')))`, w.ID, w.ID, w.ID, b.LauncherID).Scan(&unsafe); err != nil {
				return err
			}
			if unsafe {
				return ErrSwarmEvalConflict
			}
			result.State = "stopped"
			result.Reason = mismatch.Error()
		} else {
			response := ""
			if answer.Response != nil {
				response = *answer.Response
			}
			if err := s.recordAnswer(tx, req, b.LauncherID, answer.Outcome, response); err != nil {
				return err
			}
			recorded, err := s.GetSwarmRequest(tx, w.ID, req.ID)
			if err != nil {
				return err
			}
			if !sameAnswer(recorded, SwarmAnswer{UserID: b.LauncherID, Outcome: answer.Outcome, Response: response}) || recorded.State != "answered" {
				return ErrSwarmEvalConflict
			}
			if err := s.advanceNodeRequest(tx, w, req, answer.Outcome); err != nil {
				return err
			}
			step, err := s.GetSwarmStep(tx, w.ID, *req.Sequence)
			if err != nil {
				return err
			}
			if step.State != "answered" || step.Answer == nil || *step.Answer != answer.Outcome {
				return ErrSwarmEvalConflict
			}
			current, err := s.GetSwarm(tx, w.ID)
			if err != nil {
				return err
			}
			doc, err := executableGraph(w.Graph)
			if err != nil {
				return err
			}
			expected, err := pipeline.Advance(doc, pipeline.Event{Node: answer.NodeID, State: "answered", Outcome: answer.Outcome})
			if err != nil {
				return err
			}
			if current.Execution != executionOfTransition(expected.Execution) {
				return ErrSwarmEvalConflict
			}
			if !expected.Terminal {
				next, err := s.GetSwarmStep(tx, w.ID, *req.Sequence+1)
				if err != nil {
					return err
				}
				if next.NodeID != expected.Next || next.State != expected.NextState {
					return ErrSwarmEvalConflict
				}
				if expected.NextState == "waiting" {
					var open bool
					if err := s.conn(tx).QueryRowContext(tx, `SELECT EXISTS(SELECT 1 FROM swarm_requests WHERE swarm_id=? AND sequence=? AND origin='node' AND state='open')`, w.ID, next.Sequence).Scan(&open); err != nil {
						return err
					}
					if !open {
						return ErrSwarmEvalConflict
					}
				}
			}
			result.State = "applied"
			result.Answer = &answer
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := s.conn(tx).ExecContext(tx, `INSERT INTO swarm_eval_replays(candidate_swarm_id,request_id,decision) VALUES(?,?,?)`, in.CandidateSwarmID, in.RequestID, string(raw)); err != nil {
			return err
		}
		got, err := s.GetSwarmEvalReplay(tx, in.CandidateSwarmID, in.RequestID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, result) {
			return fmt.Errorf("%w: replay decision was not retained", ErrSwarmEvalConflict)
		}
		return nil
	})
	if err != nil {
		return SwarmEvalReplay{}, err
	}
	return result, nil
}

func (s *Store) authorizeEvalOwnerStop(ctx context.Context, b SwarmEvalBinding, authorize SwarmEvalAuthorize) error {
	if err := s.authorizeEval(ctx, "interrupt_eval_children", b, authorize); err != nil {
		return err
	}
	user, err := s.GetUser(ctx, b.LauncherID)
	if err != nil || user.Disabled {
		return ErrSwarmRequestDenied
	}
	if user.IsAdmin {
		return nil
	}
	role, err := s.GetSwarmRole(ctx, b.CandidateSwarmID, b.LauncherID)
	if err != nil || role != "owner" {
		return ErrSwarmRequestDenied
	}
	return nil
}
