package store

import (
	"context"
	"errors"
)

// failureCleanupSource reads positive immutable facts only. Missing reports,
// seals, capacity or actor diagnostics never select this lifecycle source.
func (s *Store) failureCleanupSource(ctx context.Context, id string) (bool, error) {
	if _, err := s.GetEphemeralChildFailure(ctx, id); err == nil {
		return true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if _, err := s.GetEphemeralChildExecutorFailure(ctx, id); err == nil {
		return true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	return false, nil
}

// Historical parent authority is deliberate: the computed aggregate join must
// already be applied, so currentChildParent's active-step check is inappropriate.
func (s *Store) ephemeralSettledCleanupScope(ctx context.Context, id, kind, action string, authorize SwarmChildAuthorize) (EphemeralChildIntent, EphemeralChildAllocation, error) {
	intent, err := s.GetEphemeralChildIntent(ctx, id)
	if err != nil {
		return intent, EphemeralChildAllocation{}, err
	}
	allocation, err := s.GetEphemeralChildAllocation(ctx, id)
	if err != nil {
		return intent, allocation, err
	}
	if authorize == nil {
		return intent, allocation, ErrSwarmChildDenied
	}
	executor := intent.Parent.Principal
	executor.ActorUID, executor.Incarnation = allocation.ActorUID, allocation.ActorIncarnation
	if err := authorize(ctx, SwarmChildAuthorization{Action: action, Caller: intent.Parent.Principal, Parent: intent.Parent, Executor: executor, RequestID: intent.RequestID, Payload: intent.Payload}); err != nil {
		return intent, allocation, err
	}
	if err := s.childIdentity(ctx, intent.Parent.Principal); err != nil {
		return intent, allocation, err
	}
	join, err := s.loadChildJoin(ctx, intent.Parent)
	if err != nil {
		return intent, allocation, err
	}
	if join.State != "open" || join.Interrupted || join.Evaluation.Open != 0 {
		return intent, allocation, ErrSwarmChildConflict
	}
	parentOutcome := "fail"
	if join.Evaluation.State == "ready_success" {
		parentOutcome = "success"
	} else if join.Evaluation.State != "ready_fail" {
		return intent, allocation, ErrSwarmChildConflict
	}
	found := false
	if kind == "cancel_cleanup" {
		if err := s.cancelCleanupEvidence(ctx, intent, allocation, join); err != nil {
			return intent, allocation, err
		}
		found = true
	} else {
		for _, failure := range join.UnregisteredFailures {
			if failure.IntentID == id && failure.RequestID == intent.RequestID {
				found = true
			}
		}
		for _, failure := range join.ExecutorFailures {
			if failure.IntentID != id || failure.RequestID != intent.RequestID {
				continue
			}
			for _, child := range join.Children {
				if child.ID != failure.ChildID || child.RequestID != intent.RequestID {
					continue
				}
				membership, err := s.GetEphemeralChildIntentForTask(ctx, child.ID)
				if err != nil {
					return intent, allocation, err
				}
				if membership.ID != id || child.Executor != executor || child.Payload != intent.Payload || child.Role != "" || child.Delivery != "" || (child.State != "pending" && child.State != "running") {
					return intent, allocation, ErrSwarmChildConflict
				}
				proof, err := s.GetEphemeralChildAttestation(ctx, id)
				if err != nil {
					return intent, allocation, err
				}
				if proof.State != "attested" || proof.ActorUID != executor.ActorUID || proof.ActorIncarnation != executor.Incarnation || proof.Origin != intent.Origin || proof.Commit != intent.SourceCommit || proof.AttestedOrigin != intent.Origin || proof.AttestedCommit != intent.SourceCommit {
					return intent, allocation, ErrPipelineSourceUnverified
				}
				found = true
			}
		}
	}
	if !found {
		return intent, allocation, ErrSwarmChildConflict
	}
	actor, err := s.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		return intent, allocation, err
	}
	if actor.Incarnation != allocation.ActorIncarnation || actor.Class != ActorClassEphemeral || actor.Role != "worker" || actor.ParentUID != intent.Parent.Principal.ActorUID || actor.SwarmID != executor.SwarmID || actor.AtespaceUID != executor.AtespaceUID || actor.PoolUID != intent.PoolUID || actor.Template != intent.Template || actor.SourceFingerprint != intent.SourceFingerprint || actor.Origin != intent.Origin {
		return intent, allocation, ErrSwarmChildConflict
	}
	swarm, err := s.GetSwarm(ctx, executor.SwarmID)
	if err != nil {
		return intent, allocation, err
	}
	visit, err := s.GetSwarmStep(ctx, swarm.ID, intent.Parent.Sequence)
	if err != nil {
		return intent, allocation, err
	}
	_, _, commit, err := s.stepPass(ctx, swarm.ID, intent.Parent.Sequence)
	if err != nil {
		return intent, allocation, err
	}
	if swarm.Source == nil || swarm.Source.Origin != intent.Origin || commit == "" || commit != intent.SourceCommit || visit.AttestedCommit != intent.SourceCommit {
		return intent, allocation, ErrPipelineSourceUnverified
	}
	if visit.Pass != intent.Parent.Pass || visit.ActorUID != intent.Parent.Principal.ActorUID || visit.ActorIncarnation != intent.Parent.Principal.Incarnation || visit.RequestID != intent.Parent.RequestID || visit.State != "completed" || visit.Outcome == nil || *visit.Outcome != parentOutcome || visit.Report != ChildJoinReport(join) {
		return intent, allocation, ErrSwarmChildConflict
	}
	return intent, allocation, nil
}

// Только положительно незапрошенные Create/first-start можно заменить до terminal
// стадии: queued Create grant либо точная start provenance с unsent marker.
// Исходный physical hash проверен scope; fake done/UPID не записывается.
func (s *Store) beginEphemeralFailureDelete(ctx context.Context, id string, a ManagedActor, operationID string) (ManagedActor, error) {
	knownStart := a.Operation == "start" && a.Stage == "start" && a.Phase == "stopped" && a.Desired == "running" && a.UPID == ""
	if knownStart {
		start, err := s.GetEphemeralChildLifecycle(ctx, a.UID, a.Incarnation, a.OperationID)
		if err != nil {
			return ManagedActor{}, err
		}
		if start.Kind != "start" || start.IntentID != id {
			return ManagedActor{}, ErrSwarmChildConflict
		}
	}
	if a.Stage != "done" && !knownStart {
		allocation, err := s.GetEphemeralChildAllocation(ctx, id)
		if err != nil {
			return ManagedActor{}, err
		}
		grant, err := s.GetEphemeralChildCreateGrant(ctx, id)
		if err != nil {
			return ManagedActor{}, err
		}
		if grant.State != "queued" || grant.ReservedAt != "" || a.Operation != "create" || a.OperationID != allocation.CreateOperationID || a.Stage != "create" || a.Phase != "provisioning" || a.Desired != "running" || a.UPID != "" || a.Error != "" {
			return ManagedActor{}, ErrSwarmChildConflict
		}
	}
	a.OperationID, a.Operation, a.Stage, a.Phase, a.Desired, a.Error, a.UPID = operationID, "delete", "stop", "deleting", "deleted", ManagedStopUnsent, ""
	a.UpdatedAt = nowRFC3339()
	result, err := s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET operation_id=?,operation='delete',stage='stop',phase='deleting',desired='deleted',error=?,upid='',revision=revision+1,updated_at=? WHERE uid=? AND revision=?`, a.OperationID, a.Error, a.UpdatedAt, a.UID, a.Revision)
	if err != nil {
		return ManagedActor{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ManagedActor{}, err
	}
	if n != 1 {
		return ManagedActor{}, ErrRevisionConflict
	}
	a.Revision++
	return a, nil
}
