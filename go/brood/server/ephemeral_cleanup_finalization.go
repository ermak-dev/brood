package server

import (
	"context"
	"reflect"

	"brood.dev/go/brood/store"
)

// completeEphemeralChildCleanup consumes positive absence evidence, not an effect
// grant. It never sends Stop/Delete. Unknown submissions retain their allocation
// even when the provider currently reports the global VMID free.
func (s *Server) completeEphemeralChildCleanup(ctx context.Context, userID string, parent store.SwarmChildParent, uid string, incarnation int64, operationID string, revision int64) (store.ManagedActor, error) {
	if s.managed == nil {
		return store.ManagedActor{}, store.ErrSwarmChildDenied
	}
	m := s.managed
	ctx, cancel := context.WithTimeout(ctx, m.actorPassTimeout())
	defer cancel()
	history, err := s.store.GetEphemeralChildLifecycle(ctx, uid, incarnation, operationID)
	if err != nil {
		return store.ManagedActor{}, err
	}
	if !ephemeralCleanupLifecycle(history.Kind) {
		return store.ManagedActor{}, store.ErrSwarmChildDenied
	}
	actor, pool, authorize, err := s.ephemeralLifecyclePreflight(ctx, userID, parent, history.IntentID, history.Kind)
	if err != nil {
		return store.ManagedActor{}, err
	}
	if actor.UID != uid || actor.Incarnation != incarnation || actor.OperationID != operationID || actor.Revision != revision || (actor.Stage != "stop" && actor.Stage != "delete") {
		return actor, store.ErrSwarmChildConflict
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	release, err := m.effects.acquire(ctx, parent.Principal.ActorUID, uid)
	if err != nil {
		return actor, err
	}
	defer release()
	check := func(ctx context.Context) error {
		current, err := s.store.GetManagedActor(ctx, uid)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, actor) {
			return store.ErrRevisionConflict
		}
		return s.store.CheckEphemeralChildLifecycle(ctx, uid, incarnation, operationID, authorize)
	}
	if err := check(ctx); err != nil {
		return actor, err
	}
	knownUnrequested := stopNotRequested(actor) || deleteNotRequested(actor)
	if actor.UPID == "" && !knownUnrequested {
		return actor, store.ErrSwarmChildConflict
	}
	client, err := m.client(pool)
	if err != nil {
		return actor, err
	}
	if actor.UPID != "" {
		if err := client.WaitTask(ctx, actor.UPID); err != nil {
			return actor, err
		}
	}
	if err := client.ConfirmAbsent(ctx, actor.CTID); err != nil {
		return actor, err
	}
	swarm, err := s.store.GetSwarm(ctx, actor.SwarmID)
	if err != nil {
		return actor, err
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
		return actor, err
	}
	out := actor
	err = s.store.WithDurableTx(ctx, func(ctx context.Context) error {
		if err := check(ctx); err != nil {
			return err
		}
		var err error
		if actor.Stage == "stop" {
			out, err = s.store.ConfirmManagedActorStage(ctx, actor.UID, actor.Revision, actor.OperationID, "stop", "absent")
			if err != nil {
				return err
			}
		}
		out, err = s.store.ConfirmManagedActorDeleted(ctx, out.UID, out.Revision, out.OperationID)
		if err != nil {
			return err
		}
		stored, err := s.store.GetManagedActor(ctx, out.UID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(stored, out) {
			return store.ErrRevisionConflict
		}
		out = stored
		return nil
	})
	if err != nil {
		return actor, err
	}
	m.verified.Delete(uid)
	m.readiness.Delete(uid)
	if m.hub != nil {
		m.hub.Disconnect(uid, incarnation)
	}
	if s.proxy != nil {
		s.proxy.RecheckNow()
	}
	return out, nil
}
