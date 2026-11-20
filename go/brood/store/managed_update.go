package store

import "context"

// CheckActorUpdate reports, without writing, why an update of a would be
// refused now: an active coordination claim involving it. It lets the server
// refuse before asking the guest; the intent repeats the check in its own
// transaction.
func (s *Store) CheckActorUpdate(ctx context.Context, a ManagedActor) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		return s.guardActorCoordination(ctx, a)
	})
}

// BeginOwnerManagedActorOperation records a lifecycle intent of the swarm
// owner. Authorize reads local authority in the same transaction; nil denies.
// The owner's intent is not held by coordination claims of other actors: the
// owner controls the worker regardless of who talks to it.
func (s *Store) BeginOwnerManagedActorOperation(ctx context.Context, a ManagedActor, operationID, operation string, authorize func(context.Context) error) (ManagedActor, error) {
	var result ManagedActor
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil {
			return newCoordinationConflict("owner authority is required")
		}
		if err := authorize(ctx); err != nil {
			return err
		}
		var err error
		result, err = s.beginManagedActorOperation(ctx, a.UID, a.Revision, operationID, operation, true)
		return err
	})
	return result, err
}
