package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// EphemeralChildExecutorFailure is an immutable server decision about a registered
// child, not an actor report, transport receipt, result seal or cleanup grant.
// All identities are derived from retained intent/allocation/proof membership.
type EphemeralChildExecutorFailure struct {
	IntentID, ChildID, RequestID, Reason, FailedAt string
}

// GetEphemeralChildExecutorFailure reads internal history without granting access
// or physical authority. Adapters must enforce current visibility separately.
func (s *Store) GetEphemeralChildExecutorFailure(ctx context.Context, id string) (EphemeralChildExecutorFailure, error) {
	var out EphemeralChildExecutorFailure
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT f.intent_id,f.child_id,e.request_id,f.reason,f.failed_at FROM ephemeral_child_executor_failures f JOIN ephemeral_child_intents e ON e.id=f.intent_id WHERE f.intent_id=?`, id).Scan(&out.IntentID, &out.ChildID, &out.RequestID, &out.Reason, &out.FailedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}

// RecordEphemeralChildExecutorFailure durably records an explicit trusted local
// decision, never inferred from time, capacity, uncertain delivery or provider
// effects. Authorization is mandatory, local-only, and repeated on exact retries.
// All nesting is refused. A broken executor need not be ready, but its original
// incarnation, proof and workspace must remain known. The dedicated executor
// must not have been manually adopted; the parent must have no current-step
// interruption or outstanding operation, not an empty lifetime control history.
// No child report/transport, reservation, result seal or lifecycle is changed.
func (s *Store) RecordEphemeralChildExecutorFailure(ctx context.Context, id, reason string, authorize SwarmChildAuthorize) error {
	if !validPipelineField(id) || strings.TrimSpace(reason) == "" || plainTextProblem(reason, MaxEphemeralChildFailureBytes) != "" {
		return ErrSwarmChildConflict
	}
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		intent, allocation, err := s.currentEphemeralChildBindingScope(ctx, id, "fail_ephemeral_executor", true, false, authorize)
		if err != nil {
			return err
		}
		if err := s.checkRegisteredEphemeralProof(ctx, intent, allocation); err != nil {
			return err
		}
		for _, uid := range []string{intent.Parent.Principal.ActorUID, allocation.ActorUID} {
			if uid == allocation.ActorUID {
				var intervened bool
				if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_interventions WHERE actor_uid=? AND incarnation=?)`, uid, allocation.ActorIncarnation).Scan(&intervened); err != nil {
					return err
				}
				if intervened {
					return ErrSwarmChildConflict
				}
			}
			if _, err := s.CurrentActorOperation(ctx, uid); err == nil {
				return ErrSwarmChildConflict
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		join, err := s.loadChildJoin(ctx, intent.Parent)
		if err != nil {
			return err
		}
		var child SwarmChildTask
		for _, c := range join.Children {
			if c.RequestID == intent.RequestID {
				child = c
				break
			}
		}
		membership, err := s.GetEphemeralChildIntentForTask(ctx, child.ID)
		if err != nil {
			return err
		}
		if membership.ID != id || child.Executor.ActorUID != allocation.ActorUID || child.Executor.Incarnation != allocation.ActorIncarnation || child.Payload != intent.Payload || child.Role != "" || child.Delivery != "" || (child.State != "pending" && child.State != "running") {
			return ErrSwarmChildConflict
		}
		old, err := s.GetEphemeralChildExecutorFailure(ctx, id)
		if err == nil {
			if old.ChildID != child.ID || old.Reason != reason {
				return ErrSwarmChildConflict
			}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		result, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO ephemeral_child_executor_failures(intent_id,child_id,reason,failed_at) VALUES(?,?,?,?)`, id, child.ID, reason, nowRFC3339())
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
		saved, err := s.GetEphemeralChildExecutorFailure(ctx, id)
		if err != nil {
			return err
		}
		if saved.ChildID != child.ID || saved.Reason != reason || saved.FailedAt == "" {
			return ErrSwarmChildConflict
		}
		return nil
	})
}

func (s *Store) registeredEphemeralFailures(ctx context.Context, p SwarmChildParent) ([]EphemeralChildExecutorFailure, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT f.intent_id,f.child_id,e.request_id,f.reason,f.failed_at FROM ephemeral_child_executor_failures f JOIN ephemeral_child_intents e ON e.id=f.intent_id WHERE e.swarm_id=? AND e.parent_sequence=? ORDER BY e.rowid LIMIT 65`, p.Principal.SwarmID, p.Sequence)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EphemeralChildExecutorFailure{}
	for rows.Next() {
		var f EphemeralChildExecutorFailure
		if err := rows.Scan(&f.IntentID, &f.ChildID, &f.RequestID, &f.Reason, &f.FailedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func evaluateSwarmChildExecutorFailures(p SwarmChildJoinPolicy, children []SwarmChildTask, unregistered []EphemeralChildFailure, failures []EphemeralChildExecutorFailure) (SwarmChildJoinEvaluation, error) {
	ordinary, err := evaluateSwarmChildJoinFailures(p, children, unregistered)
	if err != nil {
		return ordinary, err
	}
	ids := map[string]bool{}
	success := 0
	for _, c := range children {
		if c.State == "completed" && c.Outcome == "success" {
			success++
		}
	}
	for _, f := range failures {
		found := false
		for _, c := range children {
			if c.ID == f.ChildID && c.RequestID == f.RequestID && (c.State == "pending" || c.State == "running") {
				found = true
				break
			}
		}
		if !found || ids[f.ChildID] || !validPipelineField(f.IntentID) || f.FailedAt == "" || strings.TrimSpace(f.Reason) == "" || plainTextProblem(f.Reason, MaxEphemeralChildFailureBytes) != "" {
			return SwarmChildJoinEvaluation{}, ErrSwarmChildConflict
		}
		ids[f.ChildID] = true
	}
	return evaluateSwarmChildCounts(p, len(children)+len(unregistered), success, p.Total-ordinary.Open-success+len(failures)), nil
}
