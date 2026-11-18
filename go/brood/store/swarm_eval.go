package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"brood.dev/go/brood/actormodel"
)

// ErrSwarmEvalConflict refuses an unsafe or changed eval selection or live path.
var ErrSwarmEvalConflict = errors.New("swarm eval conflict")

// SwarmEvalBindingInput selects a finished reference pass and an unused candidate.
// Model is the explicit validated model string; Store does not resolve aliases
// or choose installation defaults.
type SwarmEvalBindingInput struct {
	CandidateSwarmID, ReferenceSwarmID string
	ReferencePass                      int64
	LauncherID, Definition, Model      string
}

// SwarmEvalBinding is immutable nonproduction history, not current authority.
// Definition bytes, goal and source remain pinned if either catalog changes.
type SwarmEvalBinding struct {
	SwarmEvalBindingInput
	CandidateProjectID, ReferenceProjectID   string
	CandidateCreatedAt, CandidateAtespaceUID string
	Origin, SourceCommit, Goal               string
	Graph, DefinitionRevision                string
	DefinitionVersion                        int64
	NonProduction                            bool
	CreatedAt                                string
}

// SwarmEvalAuthorize must check current local authority for both scopes using
// the supplied transaction context. No network, app.Can or external effect is
// allowed. Actions are bind_eval, start_eval, replay_eval and the separate
// interrupt_eval_children owner gate. Nil always denies.
type SwarmEvalAuthorize func(context.Context, string, SwarmEvalBinding) error

// GetSwarmEvalBinding reads the retained marker for trusted effect guards. A
// binding remains nonproduction after stopping or deleting its swarm. This is
// historical data, not authorization to start, replay, land or destroy anything.
func (s *Store) GetSwarmEvalBinding(ctx context.Context, swarmID string) (SwarmEvalBinding, error) {
	var raw string
	var nonproduction bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT binding,nonproduction FROM swarm_eval_bindings WHERE candidate_swarm_id=?`, swarmID).Scan(&raw, &nonproduction)
	if errors.Is(err, sql.ErrNoRows) {
		return SwarmEvalBinding{}, ErrNotFound
	}
	if err != nil {
		return SwarmEvalBinding{}, err
	}
	var b SwarmEvalBinding
	if err = json.Unmarshal([]byte(raw), &b); err != nil {
		return SwarmEvalBinding{}, err
	}
	if b.CandidateSwarmID != swarmID || !b.NonProduction || !nonproduction {
		return SwarmEvalBinding{}, ErrSwarmEvalConflict
	}
	return b, nil
}

func (s *Store) refuseSwarmEval(ctx context.Context, swarmID string) error {
	_, err := s.GetSwarmEvalBinding(ctx, swarmID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: bound candidate requires eval-only execution", ErrSwarmEvalConflict)
}

// CreateSwarmEvalBinding commits an immutable binding before any candidate actor
// or graph exists. Exact retries reauthorize but never rewrite the selection.
// The separate FULL transaction rejects all nesting, including exact retries.
func (s *Store) CreateSwarmEvalBinding(ctx context.Context, in SwarmEvalBindingInput, authorize SwarmEvalAuthorize) (SwarmEvalBinding, error) {
	var result SwarmEvalBinding
	err := s.WithDurableTx(ctx, func(tx context.Context) error {
		if authorize == nil {
			return ErrSwarmRequestDenied
		}
		if !validPipelineField(in.CandidateSwarmID) || !validPipelineField(in.ReferenceSwarmID) || !validPipelineField(in.LauncherID) || !validPipelineField(in.Definition) || in.ReferencePass < 1 || !actormodel.ValidName(in.Model) {
			return ErrSwarmEvalConflict
		}
		old, err := s.GetSwarmEvalBinding(tx, in.CandidateSwarmID)
		if err == nil {
			if old.SwarmEvalBindingInput != in {
				return ErrSwarmEvalConflict
			}
			if err := s.authorizeEval(tx, "bind_eval", old, authorize); err != nil {
				return err
			}
			result = old
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		candidate, err := s.requireSwarmManager(tx, in.CandidateSwarmID, in.LauncherID)
		if err != nil {
			return err
		}
		reference, err := s.GetSwarmPassTrace(tx, in.ReferenceSwarmID, in.ReferencePass)
		if err != nil {
			return err
		}
		if candidate.ProjectID == reference.ProjectID || candidate.ID == reference.SwarmID || reference.Goal == "" || reference.SourceCommit == "" || reference.Origin == "" || (reference.Current && activeExecution(reference.CurrentExecution)) {
			return ErrSwarmEvalConflict
		}
		if reference.Current && reference.CurrentExecution != "succeeded" && reference.CurrentExecution != "failed" && reference.CurrentExecution != "stopped" {
			return ErrSwarmEvalConflict
		}
		def, err := s.GetWorkflowDefinition(tx, candidate.ProjectID, in.Definition)
		if err != nil {
			return err
		}
		if _, err := executableGraph([]byte(def.YAML)); err != nil {
			return err
		}
		namespace := ""
		if candidate.AtespaceUID != nil {
			namespace = *candidate.AtespaceUID
		}
		b := SwarmEvalBinding{SwarmEvalBindingInput: in, CandidateProjectID: candidate.ProjectID, ReferenceProjectID: reference.ProjectID, CandidateCreatedAt: *candidate.CreatedAt, CandidateAtespaceUID: namespace, Origin: reference.Origin, SourceCommit: reference.SourceCommit, Goal: reference.Goal, Graph: def.YAML, DefinitionRevision: def.Revision, DefinitionVersion: def.Version, NonProduction: true, CreatedAt: nowRFC3339()}
		if err := s.authorizeEval(tx, "bind_eval", b, authorize); err != nil {
			return err
		}
		if err := s.checkEvalUnused(tx, b); err != nil {
			return err
		}
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		if _, err = s.conn(tx).ExecContext(tx, `INSERT INTO swarm_eval_bindings(candidate_swarm_id,binding,nonproduction) VALUES(?,?,1)`, b.CandidateSwarmID, string(raw)); err != nil {
			return err
		}
		result, err = s.GetSwarmEvalBinding(tx, b.CandidateSwarmID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(result, b) {
			return ErrSwarmEvalConflict
		}
		return nil
	})
	if err != nil {
		return SwarmEvalBinding{}, err
	}
	return result, nil
}

func (s *Store) checkEvalUnused(ctx context.Context, b SwarmEvalBinding) error {
	w, err := s.GetSwarm(ctx, b.CandidateSwarmID)
	if err != nil {
		return err
	}
	if w.ProjectID != b.CandidateProjectID || w.Graph != nil || w.Execution != "idle" {
		return ErrSwarmEvalConflict
	}
	var used bool
	// Tombstones намеренно включены: отсутствие live inventory не значит never booted.
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_actors WHERE swarm_id=?) OR EXISTS(SELECT 1 FROM swarm_steps WHERE swarm_id=?) OR EXISTS(SELECT 1 FROM swarm_requests WHERE swarm_id=?) OR EXISTS(SELECT 1 FROM swarm_role_bindings WHERE swarm_id=?) OR EXISTS(SELECT 1 FROM pipeline_sources WHERE swarm_id=?)`, w.ID, w.ID, w.ID, w.ID, w.ID).Scan(&used); err != nil {
		return err
	}
	if used {
		return fmt.Errorf("%w: candidate has actor or execution history", ErrSwarmEvalConflict)
	}
	return nil
}

func (s *Store) authorizeEval(ctx context.Context, action string, b SwarmEvalBinding, authorize SwarmEvalAuthorize) error {
	if authorize == nil {
		return ErrSwarmRequestDenied
	}
	if err := s.checkEvalScope(ctx, b); err != nil {
		return err
	}
	if err := authorize(ctx, action, b); err != nil {
		return err
	}
	return s.checkEvalScope(ctx, b)
}

func (s *Store) checkEvalScope(ctx context.Context, b SwarmEvalBinding) error {
	w, err := s.requireSwarmManager(ctx, b.CandidateSwarmID, b.LauncherID)
	if err != nil {
		return err
	}
	namespace := ""
	if w.AtespaceUID != nil {
		namespace = *w.AtespaceUID
	}
	if w.ProjectID != b.CandidateProjectID || w.CreatedAt == nil || *w.CreatedAt != b.CandidateCreatedAt || namespace != b.CandidateAtespaceUID {
		return fmt.Errorf("%w: candidate identity or namespace changed", ErrSwarmEvalConflict)
	}
	// Это узкий Store API: launcher также должен управлять reference, а callback
	// дополнительно проверяет актуальную локальную сессию и namespace authority.
	ref, err := s.requireSwarmManager(ctx, b.ReferenceSwarmID, b.LauncherID)
	if err != nil {
		return err
	}
	if ref.ProjectID != b.ReferenceProjectID {
		return ErrSwarmEvalConflict
	}
	for _, project := range []string{b.CandidateProjectID, b.ReferenceProjectID} {
		origin, err := s.ProjectRepository(ctx, project)
		if err != nil {
			return err
		}
		if origin != b.Origin {
			return ErrSwarmEvalConflict
		}
	}
	first, err := s.GetSwarmStep(ctx, b.ReferenceSwarmID, b.ReferencePass)
	if err != nil {
		return err
	}
	if first.Pass != b.ReferencePass || first.Goal != b.Goal || first.SourceCommit != b.SourceCommit || ref.Source == nil || ref.Source.Origin != b.Origin {
		return ErrSwarmEvalConflict
	}
	return nil
}

// StartSwarmEval starts exactly one pass from the immutable binding in a separate
// FULL transaction. Candidate actors may now exist, but cannot change the source,
// goal, selected graph or model marker. No actors or external effects are created.
func (s *Store) StartSwarmEval(ctx context.Context, swarmID, launcherID string, authorize SwarmEvalAuthorize) (Swarm, error) {
	var result Swarm
	err := s.WithDurableTx(ctx, func(tx context.Context) error {
		b, err := s.GetSwarmEvalBinding(tx, swarmID)
		if err != nil {
			return err
		}
		if launcherID != b.LauncherID {
			return ErrSwarmRequestDenied
		}
		if err := s.authorizeEval(tx, "start_eval", b, authorize); err != nil {
			return err
		}
		w, err := s.GetSwarm(tx, swarmID)
		if err != nil {
			return err
		}
		var hasPass bool
		if err := s.conn(tx).QueryRowContext(tx, `SELECT EXISTS(SELECT 1 FROM swarm_steps WHERE swarm_id=?)`, swarmID).Scan(&hasPass); err != nil {
			return err
		}
		if hasPass || w.Graph != nil || w.Execution != "idle" {
			return fmt.Errorf("%w: eval permits only its first pass", ErrSwarmEvalConflict)
		}
		// Attach пинит именно выбранные bytes, не сегодняшний каталог.
		if err := s.AttachSwarmGraph(tx, swarmID, []byte(b.Graph), b.Definition, b.DefinitionVersion, b.DefinitionRevision, b.Origin); err != nil {
			return err
		}
		result, err = s.startSwarm(tx, SwarmStart{SwarmID: swarmID, UserID: launcherID, Definition: b.Definition, Commit: b.SourceCommit, Origin: b.Origin, Goal: b.Goal}, func(context.Context) bool { return true }, true)
		if err != nil {
			return err
		}
		first, err := s.GetSwarmStep(tx, swarmID, 1)
		if err != nil {
			return err
		}
		if first.Goal != b.Goal || first.SourceCommit != b.SourceCommit || string(result.Graph) != b.Graph || !activeExecution(result.Execution) || result.Source == nil || result.Source.Origin != b.Origin || result.Source.Commit != b.SourceCommit {
			return ErrSwarmEvalConflict
		}
		policy, err := s.SwarmStepWorkflowPolicy(tx, swarmID, 1)
		if err != nil {
			return err
		}
		if policy.Pass != 1 || policy.ProjectID != b.CandidateProjectID || policy.LegacyEmpty {
			return ErrSwarmEvalConflict
		}
		return nil
	})
	if err != nil {
		return Swarm{}, err
	}
	return result, nil
}
