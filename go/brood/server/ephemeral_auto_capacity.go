package server

import (
	"context"
	"errors"
	"reflect"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// Admission создаёт только metadata. Ни выбранный свободный VMID, ни возвращённая
// историческая allocation не заменяют отдельное свежее FULL разрешение Create.
func (s *Server) allocateQueuedEphemeralChild(ctx context.Context, intentID string, hint, maxChecks int) (store.EphemeralChildAllocation, int, error) {
	var empty store.EphemeralChildAllocation
	if s.managed == nil {
		return empty, 0, store.ErrSwarmChildDenied
	}
	ctx, cancel := context.WithTimeout(ctx, s.managed.actorPassTimeout())
	defer cancel()
	intent, err := s.store.GetEphemeralChildIntent(ctx, intentID)
	if err != nil {
		return empty, 0, err
	}
	parent, err := s.store.GetManagedActor(ctx, intent.Parent.Principal.ActorUID)
	if err != nil {
		return empty, 0, err
	}
	pool, err := s.store.GetManagedPool(ctx, intent.PoolUID)
	if err != nil {
		return empty, 0, err
	}
	if pool.State != "enabled" {
		return empty, 0, store.ErrPoolUnavailable
	}
	resources, err := managedActorResources(pool)
	if err != nil {
		return empty, 0, err
	}
	op := &autoEphemeralCapacityOperation{ephemeralCapacityOperation: ephemeralCapacityOperation{server: s, context: ctx, userID: parent.DelegatorID, intent: intent, reservation: store.EphemeralActorReservation{Resources: resources}}, pool: pool, hint: hint, maxChecks: maxChecks}
	op.ephemeralCapacityOperation.poolSnapshot = &op.pool
	err = s.app.AdmitWorkload(parent.DelegatorID, intent.Parent.Principal.SwarmID, op)
	return op.saved, op.nextHint, err
}

type autoEphemeralCapacityOperation struct {
	ephemeralCapacityOperation
	pool                      store.ManagedPool
	hint, maxChecks, nextHint int
}

func (o *autoEphemeralCapacityOperation) Apply(selector map[string]string) error {
	m := o.server.managed
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	existing, err := o.server.store.GetEphemeralChildAllocation(o.context, o.intent.ID)
	if err == nil {
		actor, err := o.server.store.GetManagedActor(o.context, existing.ActorUID)
		if err != nil {
			return err
		}
		if actor.Incarnation != existing.ActorIncarnation {
			return store.ErrSwarmChildConflict
		}
		o.reservation = store.EphemeralActorReservation{UID: actor.UID, OwnerNonce: actor.OwnerNonce, OperationID: existing.CreateOperationID, CTID: actor.CTID, Resources: actor.Resources}
		return o.ephemeralCapacityOperation.Apply(selector)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	current, err := o.server.store.GetManagedPool(o.context, o.pool.UID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, o.pool) {
		return store.ErrRevisionConflict
	}
	if current.State != "enabled" {
		return store.ErrPoolUnavailable
	}
	actors, err := o.server.store.ListManagedActors(o.context, "")
	if err != nil {
		return err
	}
	reserved := map[int]struct{}{}
	var count int64
	for _, actor := range actors {
		if actor.Phase == "deleted" {
			continue
		}
		if actor.PoolUID == current.UID {
			count++
		}
		if actor.ClusterIdentity == current.Source.ClusterIdentity {
			reserved[actor.CTID] = struct{}{}
		}
	}
	if count >= current.MaxActors {
		return store.ErrPoolCapacity
	}
	client, err := m.client(current)
	if err != nil {
		return err
	}
	choice, err := proxmox.SelectAvailableVMID(o.context, client, reserved, o.hint, o.maxChecks)
	o.nextHint = choice.ResumeAt
	if err != nil {
		return err
	}
	swarm, err := o.server.store.GetSwarm(o.context, o.intent.Parent.Principal.SwarmID)
	if err != nil {
		return err
	}
	if err := o.server.checkSwarmNamespace(o.context, toAppSwarm(swarm)); err != nil {
		return err
	}
	o.reservation.UID = store.NewID()
	o.reservation.OwnerNonce = store.NewID()
	o.reservation.OperationID = store.NewID()
	o.reservation.CTID = choice.ID
	return o.ephemeralCapacityOperation.Apply(selector)
}
