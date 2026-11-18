package store

import "context"

// ContinueOwnerManagedActorReset records the first start of an accepted owner
// reset after its new incarnation's Create has been reconciled. It requires an
// exact retained derived reset link and a local, transaction-only callback.
// This continues an existing owner intent without creating another intervention
// or clearing automatic coordination holds. It does not submit a provider effect.
func (s *Store) ContinueOwnerManagedActorReset(ctx context.Context, target ManagedActor, operationID string, authorize func(context.Context, OwnerManagedActorLineage) error) (ManagedActor, error) {
	var result ManagedActor
	if authorize == nil || operationID == "" || operationID == target.OperationID {
		return result, ErrSwarmChildDenied
	}
	err := s.withInterventionTx(ctx, func(ctx context.Context) error {
		link, err := s.ownerResetContinuation(ctx, target)
		if err != nil {
			return err
		}
		if err := authorize(ctx, link); err != nil {
			return err
		}
		current, err := s.ownerResetContinuation(ctx, target)
		if err != nil {
			return err
		}
		if current != link {
			return ErrRevisionConflict
		}
		next, err := s.beginManagedActorOperation(ctx, target.UID, target.Revision, operationID, "start", true)
		if err != nil {
			return err
		}
		if err := s.retainOwnerManagedActorLineage(ctx, target, next); err != nil {
			return err
		}
		result = next
		return nil
	})
	if err != nil {
		return ManagedActor{}, err
	}
	return result, nil
}

func (s *Store) ownerResetContinuation(ctx context.Context, target ManagedActor) (OwnerManagedActorLineage, error) {
	var empty OwnerManagedActorLineage
	current, err := s.GetManagedActor(ctx, target.UID)
	if err != nil {
		return empty, err
	}
	if current != target {
		return empty, ErrRevisionConflict
	}
	if current.Operation != "reset" || current.Stage != "done" || current.Phase != "stopped" || current.Desired != "running" || current.DeletedAt != nil {
		return empty, ErrManagedBinding
	}
	link, err := s.GetDerivedOwnerManagedActorOperation(ctx, current.UID, current.Incarnation, current.OperationID)
	if err != nil {
		return empty, err
	}
	swarm, err := s.GetSwarm(ctx, current.SwarmID)
	if err != nil {
		return empty, err
	}
	if swarm.State != "active" || swarm.AtespaceUID == nil || *swarm.AtespaceUID != current.AtespaceUID || link.Operation != "reset" || link.Origin.Operation != "reset" || link.Origin.State != "completed" || link.Origin.UID != current.UID || link.Origin.SwarmID != swarm.ID || link.Origin.ProjectID != swarm.ProjectID {
		return empty, ErrSwarmChildDenied
	}
	return link, nil
}
