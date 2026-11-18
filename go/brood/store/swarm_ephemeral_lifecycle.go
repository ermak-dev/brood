package store

import (
	"context"
	"database/sql"
	"errors"
)

// EphemeralChildLifecycle is server-only immutable automatic-operation history.
// Absence is not permission to infer automatic provenance from class or phase.
// It exposes no payload, workspace lease or physical ownership credentials.
type EphemeralChildLifecycle struct {
	IntentID, Kind, OperationID, ActorUID string
	ActorIncarnation                      int64
	CreatedAt                             string
}

// GetEphemeralChildLifecycle reads exact historical provenance, including after
// inventory removal. History is neither current authority nor effect permission.
func (s *Store) GetEphemeralChildLifecycle(ctx context.Context, uid string, inc int64, operationID string) (EphemeralChildLifecycle, error) {
	var out EphemeralChildLifecycle
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT l.intent_id,l.kind,l.operation_id,a.actor_uid,a.actor_incarnation,l.created_at FROM ephemeral_child_lifecycles l JOIN ephemeral_child_allocations a ON a.intent_id=l.intent_id WHERE a.actor_uid=? AND a.actor_incarnation=? AND l.operation_id=?`, uid, inc, operationID).Scan(&out.IntentID, &out.Kind, &out.OperationID, &out.ActorUID, &out.ActorIncarnation, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EphemeralChildLifecycle{}, ErrNotFound
	}
	if err != nil {
		return EphemeralChildLifecycle{}, err
	}
	return out, nil
}

// Общие physical-проверки используют исходный create ID, не текущий start/delete.
func (s *Store) ephemeralLifecycleScope(ctx context.Context, id, kind, action string, authorize SwarmChildAuthorize) (ManagedActor, error) {
	var intent EphemeralChildIntent
	var allocation EphemeralChildAllocation
	var err error
	if kind == "start" {
		// Read-only check наблюдает и registered executor; новые grants
		// по-прежнему требуют отсутствия ребёнка и свежего stage reserve.
		intent, allocation, err = s.currentEphemeralChildBindingScope(ctx, id, action, action == "check_ephemeral_start", false, authorize)
		if err != nil {
			return ManagedActor{}, err
		}
		grant, err := s.GetEphemeralChildCreateGrant(ctx, id)
		if err != nil {
			return ManagedActor{}, err
		}
		if grant.State != "reserved" {
			return ManagedActor{}, ErrSwarmChildConflict
		}
	} else if kind == "failure_cleanup" || kind == "cancel_cleanup" {
		intent, allocation, err = s.ephemeralSettledCleanupScope(ctx, id, kind, action, authorize)
		if err != nil {
			return ManagedActor{}, err
		}
	} else if kind == "cleanup" {
		if authorize == nil {
			return ManagedActor{}, ErrSwarmChildDenied
		}
		// Seal проверяется, но никогда не создаётся и не обновляется этим путём.
		retained, err := s.GetEphemeralChildResultSeal(ctx, id)
		if err != nil {
			return ManagedActor{}, err
		}
		current, err := s.currentEphemeralResult(ctx, id, func(ctx context.Context, a SwarmChildAuthorization) error {
			a.Action = action
			return authorize(ctx, a)
		})
		if err != nil {
			return ManagedActor{}, err
		}
		current.CreatedAt = retained.CreatedAt
		if current != retained {
			return ManagedActor{}, ErrSwarmChildConflict
		}
		intent, err = s.GetEphemeralChildIntent(ctx, id)
		if err != nil {
			return ManagedActor{}, err
		}
		allocation, err = s.GetEphemeralChildAllocation(ctx, id)
		if err != nil {
			return ManagedActor{}, err
		}
		swarm, err := s.GetSwarm(ctx, intent.Parent.Principal.SwarmID)
		if err != nil {
			return ManagedActor{}, err
		}
		if swarm.Source == nil || swarm.Source.Origin != intent.Origin {
			return ManagedActor{}, ErrPipelineSourceUnverified
		}
	} else {
		return ManagedActor{}, ErrSwarmChildConflict
	}
	a, err := s.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		return ManagedActor{}, err
	}
	if allocation.ActorIncarnation != 1 || a.Incarnation != 1 || a.Phase == "deleted" {
		return ManagedActor{}, ErrSwarmChildConflict
	}
	if err := s.checkEphemeralPhysicalBinding(ctx, intent, allocation, a, kind == "start"); err != nil {
		return ManagedActor{}, err
	}
	// Вмешательство, даже завершённое, навсегда ограждает эту инкарнацию
	// от автоматической работы; явные owner/manual API не меняются.
	var intervened bool
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_interventions WHERE (actor_uid=? AND incarnation=?) OR (actor_uid=? AND incarnation=?))`, a.UID, a.Incarnation, intent.Parent.Principal.ActorUID, intent.Parent.Principal.Incarnation).Scan(&intervened); err != nil {
		return ManagedActor{}, err
	}
	if intervened {
		return ManagedActor{}, ErrSwarmChildConflict
	}
	for _, uid := range []string{a.UID, intent.Parent.Principal.ActorUID} {
		if _, err := s.CurrentActorOperation(ctx, uid); err == nil {
			return ManagedActor{}, ErrSwarmChildConflict
		} else if !errors.Is(err, ErrNotFound) {
			return ManagedActor{}, err
		}
	}
	operation := "start"
	if kind != "start" {
		operation = "delete"
	}
	failedChildID := ""
	if kind == "failure_cleanup" {
		if f, err := s.GetEphemeralChildExecutorFailure(ctx, id); err == nil {
			failedChildID = f.ChildID
		} else if !errors.Is(err, ErrNotFound) {
			return ManagedActor{}, err
		}
	}
	if kind == "start" && action == "check_ephemeral_start" {
		if err := s.guardActorCoordinationClaimsForIntent(ctx, a, operation); err != nil {
			return ManagedActor{}, err
		}
		return a, nil
	}
	if err := s.guardActorCoordinationExceptChild(ctx, a, operation, failedChildID); err != nil {
		return ManagedActor{}, err
	}
	return a, nil
}

// BeginEphemeralChildStart commits automatic first-start provenance and the known
// not-requested start stage in one non-nested FULL transaction. The mandatory
// begin_ephemeral_start callback uses only local Store reads with its tx context;
// namespace/project authority must already have been checked by the server.
// The executor need not be ready. Reconciled original Create plus a retained UPID
// and reserved create grant are mandatory. Exact retries reauthorize the same
// current operation and never replace manual operations or mint fresh permission.
func (s *Store) BeginEphemeralChildStart(ctx context.Context, intentID string, authorize SwarmChildAuthorize) (ManagedActor, error) {
	return s.beginEphemeralLifecycle(ctx, intentID, "start", authorize)
}

// BeginEphemeralChildCleanup commits automatic delete intent (initial unsent stop
// stage) and provenance in one non-nested FULL transaction. It requires an already
// retained exact result seal and applied computed join, or an explicit retained
// server failure and applied computed aggregate join. A registered child cancelled
// before any instruction uses distinct cancel_cleanup provenance: exact pending
// cancellation evidence, no typed/legacy transport, and no independent result.
// Failure uses failure_cleanup; neither source fabricates a report or result seal.
// begin_ephemeral_cleanup authorizes the historical completed parent, not the
// latest active graph step.
// No reports, seals, capacity or provider state are changed. Draining is allowed.
func (s *Store) BeginEphemeralChildCleanup(ctx context.Context, intentID string, authorize SwarmChildAuthorize) (ManagedActor, error) {
	return s.beginEphemeralLifecycle(ctx, intentID, "cleanup", authorize)
}

func (s *Store) beginEphemeralLifecycle(ctx context.Context, id, kind string, authorize SwarmChildAuthorize) (ManagedActor, error) {
	if !validPipelineField(id) {
		return ManagedActor{}, ErrSwarmChildConflict
	}
	var out ManagedActor
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		action := "begin_ephemeral_" + kind
		if kind == "cleanup" {
			failed, err := s.failureCleanupSource(ctx, id)
			if err != nil {
				return err
			}
			if failed {
				kind = "failure_cleanup"
			} else {
				cancelled, err := s.cancelCleanupSource(ctx, id)
				if err != nil {
					return err
				}
				if cancelled {
					kind = "cancel_cleanup"
				}
			}
		}
		a, err := s.ephemeralLifecycleScope(ctx, id, kind, action, authorize)
		if err != nil {
			return err
		}
		var operationID string
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT operation_id FROM ephemeral_child_lifecycles WHERE intent_id=? AND kind=?`, id, kind).Scan(&operationID)
		if err == nil {
			if _, err := s.currentEphemeralLifecycleOperation(ctx, a.UID, a.Incarnation, operationID); err != nil {
				return err
			}
			out = a
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		allocation, err := s.GetEphemeralChildAllocation(ctx, id)
		if err != nil {
			return err
		}
		operation := "delete"
		if kind == "start" {
			if a.Operation != "create" || a.OperationID != allocation.CreateOperationID || a.Stage != "done" || a.Phase != "stopped" || a.Desired != "running" || a.UPID == "" {
				return ErrSwarmChildConflict
			}
			operation = "start"
		}
		operationID = NewID()
		if kind == "failure_cleanup" {
			a, err = s.beginEphemeralFailureDelete(ctx, id, a, operationID)
		} else {
			a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, operationID, operation)
		}
		if err != nil {
			return err
		}
		if kind == "start" {
			a.Phase = "stopped"
			a, err = s.UpdateManagedActor(ctx, a, a.Revision)
			if err != nil {
				return err
			}
		}
		stored, err := s.GetManagedActor(ctx, a.UID)
		if err != nil {
			return err
		}
		if stored != a {
			return ErrRevisionConflict
		}
		result, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO ephemeral_child_lifecycles(intent_id,kind,operation_id,created_at) VALUES(?,?,?,?)`, id, kind, operationID, nowRFC3339())
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
		out = a
		return nil
	})
	if err != nil {
		return ManagedActor{}, err
	}
	return out, nil
}

func (s *Store) currentEphemeralLifecycleOperation(ctx context.Context, uid string, inc int64, operationID string) (EphemeralChildLifecycle, error) {
	h, err := s.GetEphemeralChildLifecycle(ctx, uid, inc, operationID)
	if err != nil {
		return EphemeralChildLifecycle{}, err
	}
	a, err := s.GetManagedActor(ctx, uid)
	if err != nil {
		return EphemeralChildLifecycle{}, err
	}
	operation := "start"
	if h.Kind != "start" {
		operation = "delete"
	}
	if a.Incarnation != inc || a.OperationID != operationID || a.Operation != operation || (h.Kind == "start" && a.Desired != "running") || (h.Kind != "start" && a.Desired != "deleted") {
		return EphemeralChildLifecycle{}, ErrSwarmChildConflict
	}
	return h, nil
}

func ephemeralLifecycleActionKind(kind string) string {
	if kind == "failure_cleanup" || kind == "cancel_cleanup" {
		return "cleanup"
	}
	return kind
}

// CheckEphemeralChildLifecycle is a read-only current local scope check, never a
// new effect grant. Actions are check_ephemeral_start/check_ephemeral_cleanup.
// The callback is mandatory and local-only, as for begin. History alone does not
// authorize effects; a provider must also hold a successful fresh stage reserve.
func (s *Store) CheckEphemeralChildLifecycle(ctx context.Context, uid string, inc int64, operationID string, authorize SwarmChildAuthorize) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		h, err := s.currentEphemeralLifecycleOperation(ctx, uid, inc, operationID)
		if err != nil {
			return err
		}
		_, err = s.ephemeralLifecycleScope(ctx, h.IntentID, h.Kind, "check_ephemeral_"+ephemeralLifecycleActionKind(h.Kind), authorize)
		return err
	})
}

// ReserveEphemeralChildLifecycleStage commits the existing inventory requested
// marker with FULL sync before one future external mutation. All nesting is
// refused, including retries. Callback actions are reserve_ephemeral_start_start,
// reserve_ephemeral_cleanup_stop and reserve_ephemeral_cleanup_delete; callbacks
// perform only local checks using tx ctx, never effects. Exact revision and a
// known not-requested stage are required. Already requested/unknown stages refuse.
// Only explicit trusted no-effect evidence through existing managed APIs may
// restore the unsent state; observation, restart and time never do. Later NORMAL
// task bookkeeping cannot erase this power-loss-durable pre-effect boundary.
func (s *Store) ReserveEphemeralChildLifecycleStage(ctx context.Context, uid string, inc int64, operationID, stage string, expectedRevision int64, authorize SwarmChildAuthorize) (ManagedActor, error) {
	var out ManagedActor
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		h, err := s.currentEphemeralLifecycleOperation(ctx, uid, inc, operationID)
		if err != nil {
			return err
		}
		a, err := s.ephemeralLifecycleScope(ctx, h.IntentID, h.Kind, "reserve_ephemeral_"+ephemeralLifecycleActionKind(h.Kind)+"_"+stage, authorize)
		if err != nil {
			return err
		}
		if a.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if a.Stage != stage || a.UPID != "" {
			return ErrSwarmChildConflict
		}
		known := false
		if h.Kind == "start" && stage == "start" {
			known = a.Operation == "start" && a.Phase == "stopped" && a.Desired == "running"
		} else if h.Kind != "start" && a.Operation == "delete" && a.Desired == "deleted" {
			switch stage {
			case "stop":
				known = a.Phase == "running" || ((a.Phase == "stopping" || a.Phase == "deleting") && a.Unsent() == ManagedStopUnsent)
			case "delete":
				known = a.Phase == "stopped" && a.Unsent() == ManagedDeleteUnsent
			}
		}
		if !known {
			return ErrSwarmChildConflict
		}
		a.Error = ""
		a.Phase = "deleting"
		if h.Kind == "start" {
			a.Phase = "provisioning"
		}
		out, err = s.UpdateManagedActor(ctx, a, a.Revision)
		if err != nil {
			return err
		}
		stored, err := s.GetManagedActor(ctx, uid)
		if err != nil {
			return err
		}
		if stored != out {
			return ErrRevisionConflict
		}
		return nil
	})
	if err != nil {
		return ManagedActor{}, err
	}
	return out, nil
}
