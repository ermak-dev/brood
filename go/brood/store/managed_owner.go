package store

import "context"

// BeginOwnerManagedActorOperation records a lifecycle intent of the swarm
// owner. Authorize reads local authority in the same transaction; nil denies.
// The owner's intent is not held by a workspace operation of the agent: the
// owner controls the agent regardless of who talks to it.
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
