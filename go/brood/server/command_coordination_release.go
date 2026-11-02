package server

import (
	"context"
	"fmt"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/store"
)

func (m *managedProvider) dispatchLeaseAction(ctx context.Context, from store.ManagedActor, sw store.Swarm, name, requestID, action string) (command.Result, error) {
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
			return fmt.Errorf("dispatch target is not in this swarm")
		}
		claim, err = m.server.store.ShowDispatchLease(ctx, sw.ProjectID, sw.ID, target.UID)
		if err != nil {
			return fmt.Errorf("dispatch allocation is not active")
		}
		if claim.RequestID != requestID || claim.Source.UID != from.UID || claim.Source.Generation != from.Incarnation || claim.HumanID != from.DelegatorID {
			return fmt.Errorf("dispatch allocation belongs to another request or owner")
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
