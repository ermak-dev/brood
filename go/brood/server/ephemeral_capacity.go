package server

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// Сохранённая привязка не разрешает запуск провайдера, регистрацию исполнения
// или доставку задания: здесь допускаются только метаданные.
func (s *Server) allocateEphemeralChild(ctx context.Context, userID string, parent store.SwarmChildParent, intentID string, reservation store.EphemeralActorReservation) (store.EphemeralChildAllocation, error) {
	intent, err := s.store.GetEphemeralChildIntent(ctx, intentID)
	if err != nil {
		return store.EphemeralChildAllocation{}, err
	}
	if intent.Parent != parent {
		return store.EphemeralChildAllocation{}, store.ErrSwarmChildDenied
	}
	op := &ephemeralCapacityOperation{server: s, context: ctx, userID: userID, intent: intent, reservation: reservation}
	err = s.app.AdmitWorkload(userID, parent.Principal.SwarmID, op)
	if err != nil {
		return store.EphemeralChildAllocation{}, err
	}
	return op.saved, nil
}

type ephemeralCapacityOperation struct {
	server       *Server
	context      context.Context
	userID       string
	intent       store.EphemeralChildIntent
	reservation  store.EphemeralActorReservation
	poolSnapshot *store.ManagedPool
	swarms       []app.Swarm
	native       []app.ActorReservation
	saved        store.EphemeralChildAllocation
}

func (o *ephemeralCapacityOperation) authorize(ctx context.Context, a store.SwarmChildAuthorization) error {
	if a.Parent != o.intent.Parent || a.Action != "allocate_ephemeral" {
		return store.ErrSwarmChildDenied
	}
	if err := o.server.authorizeSwarmOwnerIntent(ctx, o.userID, a.Parent.Principal.SwarmID); err != nil {
		return err
	}
	if o.server.managed == nil {
		return store.ErrSwarmChildDenied
	}
	if o.poolSnapshot != nil {
		pool, err := o.server.store.GetManagedPool(ctx, o.intent.PoolUID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(pool, *o.poolSnapshot) {
			return store.ErrRevisionConflict
		}
	}
	return o.server.managed.childAuthority(ctx, a)
}

func (o *ephemeralCapacityOperation) Prepare(swarms []app.Swarm) (app.WorkloadAdmission, error) {
	plan := app.WorkloadAdmission{Name: "e-" + strconv.FormatInt(o.intent.Parent.Sequence, 36) + "-" + strings.ToLower(o.intent.ID), CPUMillicores: o.reservation.Resources.CPUMillicores, MemoryBytes: o.reservation.Resources.MemoryBytes}
	if err := o.authorize(o.context, store.SwarmChildAuthorization{Action: "allocate_ephemeral", Parent: o.intent.Parent}); err != nil {
		return plan, err
	}
	o.swarms = swarms
	var err error
	// Сеть допускается только вне транзакции, под общей блокировкой проекта.
	o.native, err = nativeShardReservations(o.context, o.server.ateapi, swarms)
	if err != nil {
		return plan, err
	}
	plan.Reservations, err = o.reservations(o.context)
	if err != nil {
		return plan, err
	}
	_, err = o.server.store.GetEphemeralChildAllocation(o.context, o.intent.ID)
	if err == nil {
		// Историческая привязка не резервирует ресурсы повторно, даже после
		// удаления актора; Store всё равно проверит права и полный отпечаток.
		plan.TemplateOnly = true
	} else if !errors.Is(err, store.ErrNotFound) {
		return plan, err
	}
	return plan, nil
}

// Внутри транзакции допустим только переданный ctx, без Gateway и сетевых чтений.
func (o *ephemeralCapacityOperation) reservations(ctx context.Context) ([]app.ActorReservation, error) {
	current, err := o.server.store.ListSwarmsByProject(ctx, o.intent.Parent.Principal.ProjectID)
	if err != nil {
		return nil, err
	}
	if len(current) != len(o.swarms) {
		return nil, app.ErrConflict
	}
	byID := make(map[string]app.Swarm, len(o.swarms))
	for _, sw := range o.swarms {
		byID[sw.ID] = sw
	}
	out := append([]app.ActorReservation{}, o.native...)
	for _, sw := range current {
		old, ok := byID[sw.ID]
		live := toAppSwarm(sw)
		if !ok || live.ProjectID != old.ProjectID || live.AtespaceName != old.AtespaceName || live.AtespaceUID != old.AtespaceUID || live.State != old.State {
			return nil, app.ErrConflict
		}
		if live.State == app.StateDeleted {
			continue
		}
		actors, err := o.server.store.ListManagedActors(ctx, sw.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range actors {
			if a.Phase == "deleted" {
				continue
			}
			if a.AtespaceUID != live.AtespaceUID {
				return nil, &shardInventoryError{cause: app.ErrConflict}
			}
			out = append(out, app.ActorReservation{SwarmID: sw.ID, Name: a.Name, CPUMillicores: a.Resources.CPUMillicores, MemoryBytes: a.Resources.MemoryBytes})
		}
	}
	return out, nil
}

func (o *ephemeralCapacityOperation) checkPool(ctx context.Context, selector map[string]string) error {
	return o.checkPoolFor(ctx, o.intent.PoolUID, selector)
}

func (o *ephemeralCapacityOperation) checkPoolFor(ctx context.Context, poolUID string, selector map[string]string) error {
	pool, err := o.server.store.GetManagedPool(ctx, poolUID)
	if err != nil {
		return err
	}
	for key, value := range selector {
		if pool.Labels[key] != value {
			return fmt.Errorf("%w: managed pool does not match required worker selector", app.ErrInvalidWorkload)
		}
	}
	return nil
}

func (o *ephemeralCapacityOperation) Apply(selector map[string]string) error {
	var err error
	o.saved, err = o.server.store.AllocateEphemeralChild(o.context, o.intent.ID, o.reservation, func(ctx context.Context, a store.SwarmChildAuthorization) error {
		if err := o.authorize(ctx, a); err != nil {
			return err
		}
		return o.checkPool(ctx, selector)
	}, func(ctx context.Context, intent store.EphemeralChildIntent, actor store.ManagedActor) error {
		if intent.ID != o.intent.ID || intent.Parent != o.intent.Parent || actor.Resources != o.reservation.Resources {
			return store.ErrSwarmChildConflict
		}
		return o.admitActor(ctx, intent, actor)
	})
	return err
}

// Каждый новый актор считается заново через ctx FULL-транзакции: предыдущие
// участники группы уже видны, внешний native-снимок остаётся тем же.
func (o *ephemeralCapacityOperation) admitActor(ctx context.Context, intent store.EphemeralChildIntent, actor store.ManagedActor) error {
	reservations, err := o.reservations(ctx)
	if err != nil {
		return err
	}
	capacity, err := o.server.store.GetProjectCapacity(ctx, intent.Parent.Principal.ProjectID)
	if err != nil {
		return err
	}
	labels, err := app.ShardWorkerSelector(capacity.WorkerSelector, nil)
	if err != nil {
		return err
	}
	if err := o.checkPoolFor(ctx, intent.PoolUID, labels); err != nil {
		return err
	}
	usage, err := app.ShardUsageFromRecords(intent.Parent.Principal.ProjectID, o.swarms, reservations)
	if err != nil {
		return err
	}
	for _, r := range reservations {
		if r.SwarmID == actor.SwarmID && r.Name == actor.Name {
			return app.ErrExists
		}
	}
	addition := app.ShardUsage{Actors: 1, CPUMillicores: actor.Resources.CPUMillicores, MemoryBytes: actor.Resources.MemoryBytes}
	if err := app.CheckShardCapacity(app.ShardCapacity{}, usage, addition); err != nil {
		return err
	}
	proposed := app.ShardUsage{Swarms: usage.Swarms, Actors: usage.Actors + 1, CPUMillicores: usage.CPUMillicores + addition.CPUMillicores, MemoryBytes: usage.MemoryBytes + addition.MemoryBytes}
	return app.CheckShardCapacityChange(app.ShardCapacity{MaxSwarms: capacity.MaxSwarms, MaxActors: capacity.MaxActors, MaxCPUMillicores: capacity.MaxCPUMillicores, MaxMemoryBytes: capacity.MaxMemoryBytes}, usage, proposed)
}
