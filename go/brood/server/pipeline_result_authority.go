package server

import (
	"context"
	"errors"

	"brood.dev/go/brood/store"
)

func (m *managedProvider) checkResultCollectorAuthority(ctx context.Context, actor store.ManagedActor, intent store.PipelineCompletionIntent, namespaceName, namespaceUID string) error {
	current, scope, err := m.authorizeCommandActorDurable(ctx, actor)
	if err != nil {
		return err
	}
	if current.UID != intent.ActorUID || current.Incarnation != intent.Incarnation || scope.AtespaceName != namespaceName || scope.AtespaceUID != namespaceUID {
		return store.ErrPipelineVisitConflict
	}
	// Явный owner abort снимает child scheduling; собственный результат актора
	// не требует после этого отозванного master delegation.
	state, err := m.server.store.SwarmChildGroupState(ctx, intent.SwarmID, intent.Sequence)
	if errors.Is(err, store.ErrNotFound) || err == nil && state == "aborted" {
		return nil
	}
	if err != nil {
		return err
	}
	parent, err := m.server.store.SwarmChildParentAt(ctx, intent.SwarmID, intent.Sequence)
	if err != nil {
		return err
	}
	return m.childAuthority(ctx, store.SwarmChildAuthorization{Action: "read", Caller: parent.Principal, Parent: parent})
}
