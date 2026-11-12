package server

import (
	"context"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/store"
)

// dispatchLeaseAction shows or releases the caller's exact dispatch request.
// It does not check ownership of the target (#267).
func (m *managedProvider) dispatchLeaseAction(ctx context.Context, from store.ManagedActor, sw store.Swarm, name, requestID, action string) (command.Result, error) {
	// Аренду, взятую на чужого актора до #267, правило не снимает, и срока у
	// неё нет: держатель отпускает её сам, без него её отпускает оператор
	// точным release (executeOperatorCoordinationCommand). Владелец worker
	// чужую аренду не отпускает: задача держателя может уже идти в сессии, и
	// снять её решает человек, а не соседний master.
	var claim store.CoordinationClaim
	err := m.server.store.WithTx(ctx, func(ctx context.Context) error {
		current, err := m.server.store.GetManagedActor(ctx, from.UID)
		if err != nil || current.Incarnation != from.Incarnation || current.Role != "master" || current.Desired != "running" || current.DelegatorID != from.DelegatorID {
			return app.ErrNotAllowed
		}
		human, err := m.server.store.GetUser(ctx, from.DelegatorID)
		if err != nil || human.Disabled || !human.IsAdmin {
			return app.ErrNotAllowed
		}
		target, err := m.server.store.LookupManagedActor(ctx, sw.ID, name)
		if err != nil {
			return command.Refuse(nil, "dispatch target is not in this swarm")
		}
		claim, err = m.server.store.ShowDispatchLease(ctx, sw.ProjectID, sw.ID, target.UID)
		if err != nil {
			return command.Refuse(nil, "dispatch allocation is not active")
		}
		if claim.RequestID != requestID || claim.Source.UID != from.UID || claim.Source.Generation != from.Incarnation || claim.HumanID != from.DelegatorID {
			return command.Refuse(nil, "dispatch allocation belongs to another request or owner")
		}
		if action == "release" {
			return m.server.store.ReleaseDispatchLease(ctx, claim)
		}
		return nil
	})
	if err != nil {
		return command.Result{}, err
	}
	return coordinationResult("Dispatch allocation "+action+"; no task was sent.", "completed", map[string]any{"request_id": requestID, "target": name, "lease_active": action != "release"})
}
