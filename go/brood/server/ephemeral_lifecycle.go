package server

import (
	"context"
	"reflect"
	"time"

	"brood.dev/go/brood/store"
)

func ephemeralCleanupLifecycle(kind string) bool {
	return kind == "cleanup" || kind == "failure_cleanup" || kind == "cancel_cleanup"
}

// ephemeralLifecyclePreflight checks external namespace authority before any Store
// transaction. Its returned callback performs local reads only, using the Store's
// transaction context. A provisioning executor is deliberately not a command target.
func (s *Server) ephemeralLifecyclePreflight(ctx context.Context, userID string, parent store.SwarmChildParent, intentID, kind string) (store.ManagedActor, store.ManagedPool, store.SwarmChildAuthorize, error) {
	deny := func(err error) (store.ManagedActor, store.ManagedPool, store.SwarmChildAuthorize, error) {
		return store.ManagedActor{}, store.ManagedPool{}, nil, err
	}
	if ephemeralCleanupLifecycle(kind) {
		kind = "cleanup"
	}
	if s.managed == nil || (kind != "start" && kind != "cleanup") {
		return deny(store.ErrSwarmChildDenied)
	}
	intent, err := s.store.GetEphemeralChildIntent(ctx, intentID)
	if err != nil {
		return deny(err)
	}
	if intent.Parent != parent {
		return deny(store.ErrSwarmChildDenied)
	}
	allocation, err := s.store.GetEphemeralChildAllocation(ctx, intentID)
	if err != nil {
		return deny(err)
	}
	actor, err := s.store.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		return deny(err)
	}
	swarm, err := s.store.GetSwarm(ctx, parent.Principal.SwarmID)
	if err != nil {
		return deny(err)
	}
	pool, err := s.store.GetManagedPool(ctx, intent.PoolUID)
	if err != nil {
		return deny(err)
	}
	authorize := func(ctx context.Context, a store.SwarmChildAuthorization) error {
		valid := a.Action == "begin_ephemeral_"+kind || a.Action == "check_ephemeral_"+kind
		if kind == "start" {
			valid = valid || a.Action == "reserve_ephemeral_start_start"
		} else {
			valid = valid || a.Action == "reserve_ephemeral_cleanup_stop" || a.Action == "reserve_ephemeral_cleanup_delete"
		}
		if !valid || a.Parent != parent {
			return store.ErrSwarmChildDenied
		}
		if err := s.authorizeSwarmOwnerIntent(ctx, userID, parent.Principal.SwarmID); err != nil {
			return err
		}
		if err := s.managed.childAuthority(ctx, a); err != nil {
			return err
		}
		origin, err := s.store.ProjectRepository(ctx, parent.Principal.ProjectID)
		if err != nil {
			return err
		}
		if origin != intent.Origin {
			return store.ErrPipelineSourceUnverified
		}
		currentIntent, err := s.store.GetEphemeralChildIntent(ctx, intentID)
		if err != nil {
			return err
		}
		currentAllocation, err := s.store.GetEphemeralChildAllocation(ctx, intentID)
		if err != nil {
			return err
		}
		currentSwarm, err := s.store.GetSwarm(ctx, swarm.ID)
		if err != nil {
			return err
		}
		currentPool, err := s.store.GetManagedPool(ctx, pool.UID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(currentIntent, intent) || currentAllocation != allocation || !reflect.DeepEqual(currentSwarm, swarm) || !reflect.DeepEqual(currentPool, pool) {
			return store.ErrSwarmChildConflict
		}
		return nil
	}
	if err := authorize(ctx, store.SwarmChildAuthorization{Action: "begin_ephemeral_" + kind, Parent: parent}); err != nil {
		return deny(err)
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
		return deny(err)
	}
	return actor, pool, authorize, nil
}

// beginEphemeralChildLifecycle creates only an automatic first-start or sealed
// cleanup intent. It performs no provider mutation and never advances an actor.
func (s *Server) beginEphemeralChildLifecycle(ctx context.Context, userID string, parent store.SwarmChildParent, intentID, kind string) (store.ManagedActor, error) {
	if s.managed == nil {
		return store.ManagedActor{}, store.ErrSwarmChildDenied
	}
	ctx, cancel := context.WithTimeout(ctx, s.managed.actorPassTimeout())
	defer cancel()
	actor, _, authorize, err := s.ephemeralLifecyclePreflight(ctx, userID, parent, intentID, kind)
	if err != nil {
		return store.ManagedActor{}, err
	}
	s.managed.lifecycle.Lock()
	defer s.managed.lifecycle.Unlock()
	release, err := s.managed.effects.acquire(ctx, parent.Principal.ActorUID, actor.UID)
	if err != nil {
		return store.ManagedActor{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return store.ManagedActor{}, err
	}
	if kind == "start" {
		return s.store.BeginEphemeralChildStart(ctx, intentID, authorize)
	}
	return s.store.BeginEphemeralChildCleanup(ctx, intentID, authorize)
}

// submitEphemeralChildLifecycleStage consumes exactly one fresh FULL stage reserve.
// It does not reconcile observations, finish stages, declare absence or release
// capacity. Requested/unknown stages are never replayed, including after restart.
func (s *Server) submitEphemeralChildLifecycleStage(ctx context.Context, userID string, parent store.SwarmChildParent, uid string, inc int64, operationID, stage string, revision int64) (store.ManagedActor, error) {
	if s.managed == nil {
		return store.ManagedActor{}, store.ErrSwarmChildDenied
	}
	m := s.managed
	ctx, cancel := context.WithTimeout(ctx, m.actorPassTimeout())
	defer cancel()
	history, err := s.store.GetEphemeralChildLifecycle(ctx, uid, inc, operationID)
	if err != nil {
		return store.ManagedActor{}, err
	}
	actor, pool, authorize, err := s.ephemeralLifecyclePreflight(ctx, userID, parent, history.IntentID, history.Kind)
	if err != nil {
		return store.ManagedActor{}, err
	}
	if actor.UID != uid {
		return store.ManagedActor{}, store.ErrSwarmChildConflict
	}
	// Порядок как у reconciliation: lifecycle, затем sorted parent/executor gates.
	// Lifecycle остаётся захвачен через физическую отправку и task bookkeeping.
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if err := ctx.Err(); err != nil {
		return store.ManagedActor{}, err
	}
	client, err := m.client(pool)
	if err != nil {
		return store.ManagedActor{}, err
	}
	release, err := m.effects.acquire(ctx, parent.Principal.ActorUID, uid)
	if err != nil {
		return store.ManagedActor{}, err
	}
	defer release()
	actor, err = s.store.GetManagedActor(ctx, uid)
	if err != nil {
		return store.ManagedActor{}, err
	}
	if actor.Incarnation != inc || actor.OperationID != operationID || actor.Stage != stage || actor.Revision != revision {
		return actor, store.ErrSwarmChildConflict
	}
	if err := s.store.CheckEphemeralChildLifecycle(ctx, uid, inc, operationID, authorize); err != nil {
		return actor, err
	}
	fresh := history.Kind == "start" && stage == "start" && startNotRequested(actor)
	if ephemeralCleanupLifecycle(history.Kind) {
		fresh = (stage == "stop" && stopNotRequested(actor)) || (stage == "delete" && deleteNotRequested(actor))
	}
	if !fresh {
		return actor, store.ErrSwarmChildConflict
	}
	// Никаких absent/running shortcuts: адаптер не согласует наблюдения.
	config, err := inspectManaged(ctx, client, actor)
	if err != nil {
		return actor, err
	}
	status, err := client.Status(ctx, actor.CTID)
	if err != nil {
		return actor, err
	}
	expected := "stopped"
	if stage == "stop" {
		expected = "running"
	}
	if status.Status != expected {
		return actor, store.ErrSwarmChildConflict
	}
	if stage == "start" {
		env := m.createSpec(actor).Env
		if !m.bootstrapCurrent(actor, env, config.Digest) {
			if err := client.ConfigureBootstrap(ctx, managedOwned(actor), env); err != nil {
				return actor, err
			}
			m.rememberBootstrap(ctx, client, actor, env)
		}
	}
	if err := ctx.Err(); err != nil {
		return actor, err
	}
	actor, err = s.store.ReserveEphemeralChildLifecycleStage(ctx, uid, inc, operationID, stage, revision, authorize)
	if err != nil {
		return actor, err
	}
	if err := s.store.CheckEphemeralChildLifecycle(ctx, uid, inc, operationID, authorize); err != nil {
		return actor, err
	}
	if err := ctx.Err(); err != nil {
		return actor, err
	}
	var upid string
	switch stage {
	case "start":
		upid, err = client.Start(ctx, managedOwned(actor))
	case "stop":
		upid, err = client.Stop(ctx, managedOwned(actor))
	case "delete":
		upid, err = client.Delete(ctx, managedOwned(actor))
	}
	if unsent(upid, err) {
		durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if stage == "delete" {
			return s.store.MarkManagedActorDeleteUnsent(durable, uid, actor.Revision, operationID, managedProviderError(err))
		}
		phase := "stopped"
		if stage == "stop" {
			phase = "running"
		}
		return m.leavePending(durable, actor, phase, err)
	}
	return m.recordTask(ctx, actor, upid, err)
}
