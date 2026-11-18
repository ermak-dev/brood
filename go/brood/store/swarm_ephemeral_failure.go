package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// MaxEphemeralChildFailureBytes bounds a server-supplied local failure decision.
const MaxEphemeralChildFailureBytes = 1024

// EphemeralChildFailure is a logical failure before child registration, not an
// executor report, source proof, physical outcome, result seal or cleanup grant.
// RequestID is read from the immutable intent, never independently supplied.
type EphemeralChildFailure struct {
	IntentID, RequestID, Reason, FailedAt string
}

// GetEphemeralChildFailure reads immutable internal history. Adapters must check
// current visibility separately; this fact never grants physical authority.
func (s *Store) GetEphemeralChildFailure(ctx context.Context, id string) (EphemeralChildFailure, error) {
	var out EphemeralChildFailure
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT f.intent_id,e.request_id,f.reason,f.failed_at FROM ephemeral_child_failures f JOIN ephemeral_child_intents e ON e.id=f.intent_id WHERE f.intent_id=?`, id).Scan(&out.IntentID, &out.RequestID, &out.Reason, &out.FailedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EphemeralChildFailure{}, ErrNotFound
	}
	if err != nil {
		return EphemeralChildFailure{}, err
	}
	return out, nil
}

func (s *Store) checkEphemeralChildNotFailed(ctx context.Context, id string) error {
	_, err := s.GetEphemeralChildFailure(ctx, id)
	if err == nil {
		return ErrSwarmChildConflict
	}
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// RecordEphemeralChildFailure records a trusted server's explicit local decision
// in a nonnested FULL transaction. It does not infer failure from time, capacity,
// RPC/PVE uncertainty or run effects. fail_ephemeral authorization is mandatory,
// local-only and rechecked on exact retries. A different reason conflicts.
// Registered children cannot use this path. Allocations, claims, grants, UPIDs,
// source evidence and physical holds remain unchanged; retry requires a new job.
func (s *Store) RecordEphemeralChildFailure(ctx context.Context, id, reason string, authorize SwarmChildAuthorize) error {
	if !validPipelineField(id) || strings.TrimSpace(reason) == "" || plainTextProblem(reason, MaxEphemeralChildFailureBytes) != "" {
		return ErrSwarmChildConflict
	}
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		intent, err := s.GetEphemeralChildIntent(ctx, id)
		if err != nil {
			return err
		}
		var executor SwarmChildPrincipal
		allocation, err := s.GetEphemeralChildAllocation(ctx, id)
		allocated := err == nil
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if allocated {
			executor = intent.Parent.Principal
			executor.ActorUID, executor.Incarnation = allocation.ActorUID, allocation.ActorIncarnation
		}
		if err := s.authorizeChild(ctx, SwarmChildAuthorization{Action: "fail_ephemeral", Caller: intent.Parent.Principal, Parent: intent.Parent, Executor: executor, RequestID: intent.RequestID, Payload: intent.Payload}, authorize); err != nil {
			return err
		}
		join, err := s.loadChildJoin(ctx, intent.Parent)
		if err != nil {
			return err
		}
		if join.State != "open" || join.Interrupted {
			return ErrSwarmChildConflict
		}
		for _, c := range join.Children {
			if c.RequestID == intent.RequestID {
				return ErrSwarmChildConflict
			}
		}
		swarm, err := s.GetSwarm(ctx, intent.Parent.Principal.SwarmID)
		if err != nil {
			return err
		}
		_, _, commit, err := s.stepPass(ctx, swarm.ID, intent.Parent.Sequence)
		if err != nil {
			return err
		}
		if swarm.Source == nil || swarm.Source.Origin != intent.Origin || commit == "" || commit != intent.SourceCommit {
			return ErrPipelineSourceUnverified
		}
		if allocated {
			if err := s.checkEphemeralChildBinding(ctx, intent, allocation, false); err != nil {
				return err
			}
		}
		principals := []SwarmChildPrincipal{intent.Parent.Principal}
		if allocated {
			principals = append(principals, executor)
		}
		for _, p := range principals {
			var intervened bool
			if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_interventions WHERE actor_uid=? AND incarnation=?)`, p.ActorUID, p.Incarnation).Scan(&intervened); err != nil {
				return err
			}
			if intervened {
				return ErrSwarmChildConflict
			}
		}
		// Не требуем отсутствия executor workspace claim: pending/unknown остаётся hold.
		if _, err := s.CurrentActorOperation(ctx, intent.Parent.Principal.ActorUID); err == nil {
			return ErrSwarmChildConflict
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		old, err := s.GetEphemeralChildFailure(ctx, id)
		if err == nil {
			if old.Reason != reason {
				return ErrSwarmChildConflict
			}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		result, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO ephemeral_child_failures(intent_id,reason,failed_at) VALUES(?,?,?)`, id, reason, nowRFC3339())
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrSwarmChildConflict
		}
		saved, err := s.GetEphemeralChildFailure(ctx, id)
		if err != nil {
			return err
		}
		if saved.Reason != reason || saved.FailedAt == "" {
			return ErrSwarmChildConflict
		}
		return nil
	})
}

func (s *Store) unregisteredEphemeralFailures(ctx context.Context, p SwarmChildParent) ([]EphemeralChildFailure, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT f.intent_id,e.request_id,f.reason,f.failed_at FROM ephemeral_child_failures f JOIN ephemeral_child_intents e ON e.id=f.intent_id WHERE e.swarm_id=? AND e.parent_sequence=? ORDER BY e.rowid LIMIT 65`, p.Principal.SwarmID, p.Sequence)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EphemeralChildFailure{}
	for rows.Next() {
		var f EphemeralChildFailure
		if err := rows.Scan(&f.IntentID, &f.RequestID, &f.Reason, &f.FailedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func evaluateSwarmChildJoinFailures(p SwarmChildJoinPolicy, children []SwarmChildTask, failures []EphemeralChildFailure) (SwarmChildJoinEvaluation, error) {
	ordinary, err := EvaluateSwarmChildJoin(p, children)
	if err != nil {
		return SwarmChildJoinEvaluation{}, err
	}
	if len(failures) == 0 {
		return ordinary, nil
	}
	if len(children)+len(failures) > p.Total {
		return SwarmChildJoinEvaluation{}, ErrSwarmChildConflict
	}
	keys := map[string]bool{}
	ids := map[string]bool{}
	for _, c := range children {
		keys[c.RequestID] = true
	}
	for _, f := range failures {
		if !validPipelineField(f.IntentID) || !validChildRequestID(f.RequestID) || f.FailedAt == "" || strings.TrimSpace(f.Reason) == "" || plainTextProblem(f.Reason, MaxEphemeralChildFailureBytes) != "" || keys[f.RequestID] || ids[f.IntentID] {
			return SwarmChildJoinEvaluation{}, ErrSwarmChildConflict
		}
		keys[f.RequestID], ids[f.IntentID] = true, true
	}
	success := 0
	for _, c := range children {
		if c.State == "completed" && c.Outcome == "success" {
			success++
		}
	}
	return evaluateSwarmChildCounts(p, len(children)+len(failures), success, p.Total-ordinary.Open-success+len(failures)), nil
}
