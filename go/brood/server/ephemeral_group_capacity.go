package server

import (
	"context"
	"fmt"
	"maps"
	"reflect"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// Групповой admission сохраняет только metadata. Проектный gate берётся раньше
// lifecycle; никакая allocation не разрешает Create или раннюю очистку.
func (s *Server) allocateQueuedEphemeralGroup(ctx context.Context, intentID string, cursors map[string]int, maxChecks int) ([]store.EphemeralChildAllocation, map[string]int, error) {
	if s.managed == nil {
		return nil, nil, store.ErrSwarmChildDenied
	}
	ctx, cancel := context.WithTimeout(ctx, s.managed.actorPassTimeout())
	defer cancel()
	intent, err := s.store.GetEphemeralChildIntent(ctx, intentID)
	if err != nil {
		return nil, nil, err
	}
	parent, err := s.store.GetManagedActor(ctx, intent.Parent.Principal.ActorUID)
	if err != nil {
		return nil, nil, err
	}
	next := maps.Clone(cursors)
	if next == nil {
		next = map[string]int{}
	}
	op := &groupEphemeralCapacityOperation{ephemeralCapacityOperation: ephemeralCapacityOperation{server: s, context: ctx, userID: parent.DelegatorID, intent: intent}, cursors: next, maxChecks: maxChecks}
	err = s.app.AdmitWorkloadContext(ctx, parent.DelegatorID, intent.Parent.Principal.SwarmID, op)
	return op.allocations, op.cursors, err
}

type groupEphemeralCapacityOperation struct {
	ephemeralCapacityOperation
	intents              []store.EphemeralChildIntent
	pools                map[string]store.ManagedPool
	reservationsByIntent map[string]store.EphemeralActorReservation
	newIntents           []store.EphemeralChildIntent
	allocations          []store.EphemeralChildAllocation
	cursors              map[string]int
	maxChecks            int
}

func (o *groupEphemeralCapacityOperation) Prepare(swarms []app.Swarm) (app.WorkloadAdmission, error) {
	var empty app.WorkloadAdmission
	history, err := o.server.store.EphemeralChildHistoryAtStep(o.context, o.intent.Parent.Principal.SwarmID, o.intent.Parent.Sequence)
	if err != nil {
		return empty, err
	}
	join, err := o.server.store.GetSwarmChildJoin(o.context, o.intent.Parent, o.server.managed.childAuthority)
	if err != nil {
		return empty, err
	}
	members := map[string]bool{}
	for _, child := range join.Children {
		members[child.RequestID] = true
	}
	slots := len(members)
	o.pools = map[string]store.ManagedPool{}
	o.reservationsByIntent = map[string]store.EphemeralActorReservation{}
	for _, h := range history {
		intent := h.Intent
		if intent.Parent != o.intent.Parent {
			return empty, store.ErrSwarmChildConflict
		}
		o.intents = append(o.intents, intent)
		if !members[intent.RequestID] {
			slots++
		}
		pool, known := o.pools[intent.PoolUID]
		if !known {
			pool, err = o.server.store.GetManagedPool(o.context, intent.PoolUID)
			if err != nil {
				return empty, err
			}
			o.pools[pool.UID] = pool
		}
		if pool.State != "enabled" {
			return empty, store.ErrPoolUnavailable
		}
		resources, err := managedActorResources(pool)
		if err != nil {
			return empty, err
		}
		reservation := store.EphemeralActorReservation{Resources: resources}
		if h.Allocation != nil {
			actor, err := o.server.store.GetManagedActor(o.context, h.Allocation.ActorUID)
			if err != nil {
				return empty, err
			}
			if actor.Incarnation != h.Allocation.ActorIncarnation {
				return empty, store.ErrSwarmChildConflict
			}
			reservation = store.EphemeralActorReservation{UID: actor.UID, OwnerNonce: actor.OwnerNonce, OperationID: h.Allocation.CreateOperationID, CTID: actor.CTID, Resources: actor.Resources}
		} else {
			o.newIntents = append(o.newIntents, intent)
		}
		o.reservationsByIntent[intent.ID] = reservation
	}
	if slots < join.Policy.Total {
		return empty, store.ErrEphemeralChildGroupPending
	}
	if slots != join.Policy.Total || len(o.intents) == 0 {
		return empty, store.ErrSwarmChildConflict
	}
	// AdmitWorkload проверяет одного реального НОВОГО актора, не retained binding.
	// Остальные новые участники проходят тот же расчёт последовательно внутри FULL.
	if len(o.newIntents) > 0 {
		o.intent = o.newIntents[0]
	} else {
		o.intent = o.intents[0]
	}
	o.reservation = o.reservationsByIntent[o.intent.ID]
	pool := o.pools[o.intent.PoolUID]
	o.poolSnapshot = &pool
	return o.ephemeralCapacityOperation.Prepare(swarms)
}

func (o *groupEphemeralCapacityOperation) authorizeGroup(ctx context.Context, a store.SwarmChildAuthorization) error {
	if a.Action != "allocate_ephemeral_group" && a.Action != "allocate_ephemeral" {
		return store.ErrSwarmChildDenied
	}
	a.Action = "allocate_ephemeral"
	if err := o.authorize(ctx, a); err != nil {
		return err
	}
	parent, err := o.server.store.GetManagedActor(ctx, o.intent.Parent.Principal.ActorUID)
	if err != nil {
		return err
	}
	if parent.DelegatorID != o.userID {
		return store.ErrSwarmChildDenied
	}
	capacity, err := o.server.store.GetProjectCapacity(ctx, o.intent.Parent.Principal.ProjectID)
	if err != nil {
		return err
	}
	selector, err := app.ShardWorkerSelector(capacity.WorkerSelector, nil)
	if err != nil {
		return err
	}
	for uid, snapshot := range o.pools {
		current, err := o.server.store.GetManagedPool(ctx, uid)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, snapshot) {
			return store.ErrRevisionConflict
		}
		if err := o.checkPoolFor(ctx, uid, selector); err != nil {
			return err
		}
	}
	return nil
}

// Счётчик общий для всех пулов и заданий. Сеть остаётся вне SQLite.
type groupVMIDRegistry struct {
	proxmox.VMIDRegistry
	checks *int
}

func (r groupVMIDRegistry) ConfirmAbsent(ctx context.Context, id int) error {
	*r.checks++
	return r.VMIDRegistry.ConfirmAbsent(ctx, id)
}

func (o *groupEphemeralCapacityOperation) Apply(selector map[string]string) error {
	m := o.server.managed
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	actors, err := o.server.store.ListManagedActors(o.context, "")
	if err != nil {
		return err
	}
	reserved := map[string]map[int]struct{}{}
	for _, pool := range o.pools {
		reserved[pool.Source.ClusterIdentity] = map[int]struct{}{}
	}
	for _, actor := range actors {
		if actor.Phase != "deleted" {
			if holds := reserved[actor.ClusterIdentity]; holds != nil {
				holds[actor.CTID] = struct{}{}
			}
		}
	}
	checks := 0
	for _, intent := range o.newIntents {
		pool := o.pools[intent.PoolUID]
		hint := o.cursors[pool.Source.ClusterIdentity]
		current, err := o.server.store.GetManagedPool(o.context, pool.UID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, pool) {
			return store.ErrRevisionConflict
		}
		for key, value := range selector {
			if pool.Labels[key] != value {
				return fmt.Errorf("%w: managed pool does not match required worker selector", app.ErrInvalidWorkload)
			}
		}
		holds := reserved[pool.Source.ClusterIdentity]
		if checks >= o.maxChecks {
			// Продолжаем с первого ещё не проверенного и не удержанного VMID.
			for hint > 0 && hint <= 999999999 {
				if _, ok := holds[hint]; !ok {
					break
				}
				hint++
			}
			if hint <= 999999999 {
				o.cursors[pool.Source.ClusterIdentity] = hint
			}
			return fmt.Errorf("PVE VMID confirmation budget exhausted after %d checks", checks)
		}
		client, err := m.client(pool)
		if err != nil {
			return err
		}
		choice, err := proxmox.SelectAvailableVMID(o.context, groupVMIDRegistry{client, &checks}, holds, hint, o.maxChecks-checks)
		o.cursors[pool.Source.ClusterIdentity] = choice.ResumeAt
		if err != nil {
			return err
		}
		holds[choice.ID] = struct{}{}
		if choice.ID < 999999999 {
			o.cursors[pool.Source.ClusterIdentity] = choice.ID + 1
		}
		r := o.reservationsByIntent[intent.ID]
		r.UID = store.NewID()
		r.OwnerNonce = store.NewID()
		r.OperationID = store.NewID()
		r.CTID = choice.ID
		o.reservationsByIntent[intent.ID] = r
	}
	sw, err := o.server.store.GetSwarm(o.context, o.intent.Parent.Principal.SwarmID)
	if err != nil {
		return err
	}
	if err := o.server.checkSwarmNamespace(o.context, toAppSwarm(sw)); err != nil {
		return err
	}
	o.allocations, err = o.server.store.AllocateEphemeralChildGroup(o.context, o.intent.Parent, o.reservationsByIntent, o.pools, o.authorizeGroup, func(ctx context.Context, intent store.EphemeralChildIntent, actor store.ManagedActor) error {
		r, ok := o.reservationsByIntent[intent.ID]
		if !ok || intent.Parent != o.intent.Parent || actor.Resources != r.Resources {
			return store.ErrSwarmChildConflict
		}
		return o.admitActor(ctx, intent, actor)
	})
	if err != nil {
		o.allocations = nil
	}
	return err
}
