package store

import "context"

// BeginOwnerAgentOperation records a lifecycle intent of the swarm
// owner. Authorize reads local authority in the same transaction; nil denies.
// The owner's intent is not held by a workspace operation of the agent: the
// owner controls the agent regardless of who talks to it.
func (s *Store) BeginOwnerAgentOperation(ctx context.Context, a Agent, operationID, operation string, authorize func(context.Context) error) (Agent, error) {
	var result Agent
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil {
			return newOperationConflict("owner authority is required")
		}
		if err := authorize(ctx); err != nil {
			return err
		}
		var err error
		result, err = s.beginAgentOperation(ctx, a.UID, a.Revision, operationID, operation, true)
		return err
	})
	return result, err
}
