package server

import (
	"context"
	"errors"
	"reflect"

	"brood.dev/go/brood/store"
)

// createAllocatedEphemeralChild consumes a fresh physical-create grant for an
// already allocated child. It neither allocates nor starts/reconciles the guest.
// A reserved grant is a permanent may-have-issued hold, not retry permission.
func (s *Server) createAllocatedEphemeralChild(ctx context.Context, userID string, parent store.SwarmChildParent, intentID string) (store.ManagedActor, error) {
	if s.managed == nil {
		return store.ManagedActor{}, store.ErrSwarmChildDenied
	}
	ctx, cancel := context.WithTimeout(ctx, s.managed.actorPassTimeout())
	defer cancel()
	intent, err := s.store.GetEphemeralChildIntent(ctx, intentID)
	if err != nil {
		return store.ManagedActor{}, err
	}
	if intent.Parent != parent {
		return store.ManagedActor{}, store.ErrSwarmChildDenied
	}
	allocation, err := s.store.GetEphemeralChildAllocation(ctx, intentID)
	if err != nil {
		return store.ManagedActor{}, err
	}
	actor, err := s.store.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		return store.ManagedActor{}, err
	}
	swarm, err := s.store.GetSwarm(ctx, parent.Principal.SwarmID)
	if err != nil {
		return store.ManagedActor{}, err
	}
	pool, err := s.store.GetManagedPool(ctx, intent.PoolUID)
	if err != nil {
		return store.ManagedActor{}, err
	}
	authorize := func(ctx context.Context, a store.SwarmChildAuthorization) error {
		if a.Action != "create_ephemeral" || a.Parent != parent {
			return store.ErrSwarmChildDenied
		}
		if err := s.authorizeSwarmOwnerIntent(ctx, userID, parent.Principal.SwarmID); err != nil {
			return err
		}
		if err := s.managed.childAuthority(ctx, a); err != nil {
			return err
		}
		// Workspace hold родителя, в том числе unknown, запрещает новую физическую
		// работу; childAuthority не проверяет этот slot для create_ephemeral.
		if _, err := s.store.CurrentActorOperation(ctx, parent.Principal.ActorUID); err == nil {
			return store.ErrSwarmChildDenied
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		origin, err := s.store.ProjectRepository(ctx, parent.Principal.ProjectID)
		if err != nil {
			return err
		}
		if origin != intent.Origin {
			return store.ErrPipelineSourceUnverified
		}
		// Снимки связывают внешнюю подготовку с последней транзакционной проверкой.
		// Исполнитель ещё provisioning: ready-only проверка command actor ему не подходит.
		currentSwarm, err := s.store.GetSwarm(ctx, swarm.ID)
		if err != nil {
			return err
		}
		currentPool, err := s.store.GetManagedPool(ctx, pool.UID)
		if err != nil {
			return err
		}
		currentActor, err := s.store.GetManagedActor(ctx, actor.UID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(currentSwarm, swarm) || !reflect.DeepEqual(currentPool, pool) || !reflect.DeepEqual(currentActor, actor) {
			return store.ErrSwarmChildConflict
		}
		return nil
	}
	// Namespace и настройка провайдера выполняются до owner gates и без SQLite tx.
	if err := authorize(ctx, store.SwarmChildAuthorization{Action: "create_ephemeral", Parent: parent}); err != nil {
		return store.ManagedActor{}, err
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
		return store.ManagedActor{}, err
	}
	s.managed.lifecycle.Lock()
	defer s.managed.lifecycle.Unlock()
	client, err := s.managed.client(pool)
	if err != nil {
		return store.ManagedActor{}, err
	}
	spec := s.managed.createSpec(actor)
	release, err := s.managed.effects.acquire(ctx, parent.Principal.ActorUID, actor.UID)
	if err != nil {
		return store.ManagedActor{}, err
	}
	defer release()
	grant, err := s.store.ReserveEphemeralChildCreate(ctx, intentID, authorize)
	if err != nil {
		return store.ManagedActor{}, err
	}
	if grant.Replay {
		return actor, nil
	}
	if err := s.store.CheckEphemeralChildCreate(ctx, intentID, authorize); err != nil {
		return store.ManagedActor{}, err
	}
	if err := ctx.Err(); err != nil {
		return store.ManagedActor{}, err
	}
	// Gate удерживается и внутри обёрток Create: вмешательство владельца не может
	// завершиться между последней проверкой и фактической отправкой. Никакого rearm.
	upid, createErr := client.Create(ctx, spec)
	return s.managed.recordTask(ctx, actor, upid, createErr)
}
