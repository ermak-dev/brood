package server

import (
	"context"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// Queued — доказанно незапрошенный Create, не истёкший unknown. Чтение
// grant никогда не выдаёт новое физическое разрешение и не повторяет Create.
func (m *managedProvider) ephemeralCreateQueued(ctx context.Context, a store.ManagedActor) (bool, error) {
	intent, err := m.server.store.GetEphemeralChildIntentForActor(ctx, a.UID, a.Incarnation)
	if err != nil {
		return false, err
	}
	allocation, err := m.server.store.GetEphemeralChildAllocation(ctx, intent.ID)
	if err != nil {
		return false, err
	}
	if a.Incarnation != 1 || allocation.ActorUID != a.UID || allocation.ActorIncarnation != a.Incarnation || allocation.CreateOperationID != a.OperationID || a.SwarmID != intent.Parent.Principal.SwarmID || a.AtespaceUID != intent.Parent.Principal.AtespaceUID || a.ParentUID != intent.Parent.Principal.ActorUID || a.PoolUID != intent.PoolUID || a.Template != intent.Template || a.SourceFingerprint != intent.SourceFingerprint || a.Origin != intent.Origin || a.Desired != "running" {
		return false, store.ErrManagedBinding
	}
	grant, err := m.server.store.GetEphemeralChildCreateGrant(ctx, intent.ID)
	if err != nil {
		return false, err
	}
	if grant.ActorUID != a.UID || grant.ActorIncarnation != a.Incarnation || grant.CreateOperationID != a.OperationID {
		return false, store.ErrManagedBinding
	}
	switch grant.State {
	case "queued":
		if a.Stage != "create" || a.Phase != "provisioning" || a.UPID != "" || a.Error != "" {
			return false, store.ErrSwarmChildConflict
		}
		return true, nil
	case "reserved":
		return false, nil
	default:
		return false, proxmox.ErrUncertain
	}
}
