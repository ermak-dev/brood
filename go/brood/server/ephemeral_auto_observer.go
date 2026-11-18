package server

import (
	"context"
	"errors"
	"fmt"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

var errEphemeralObservationHeld = errors.New("automatic ephemeral observation held")

// Класс и отсутствие automatic provenance не дают manual authority.
func (m *managedProvider) reconcileEphemeralOperation(ctx context.Context, actor store.ManagedActor) (store.ManagedActor, bool, error) {
	if _, err := m.server.ownerActorOperationOrigin(ctx, actor); err == nil {
		return actor, false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return actor, true, fmt.Errorf("%w: %w", errEphemeralObservationHeld, err)
	}
	history, err := m.server.store.GetEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID)
	if err != nil {
		return actor, true, fmt.Errorf("%w: %w", errEphemeralObservationHeld, err)
	}
	out, err := m.observeEphemeralLifecycle(ctx, actor, history)
	if err != nil && !errors.Is(err, errEphemeralObservationHeld) {
		err = fmt.Errorf("%w: %w", errEphemeralObservationHeld, err)
	}
	return out, true, err
}

func (m *managedProvider) retainEphemeralObservationFailure(ctx context.Context, actor store.ManagedActor, check func(context.Context) error, cause error) (store.ManagedActor, error) {
	held := func(err error) (store.ManagedActor, error) {
		return actor, fmt.Errorf("%w: %w", errEphemeralObservationHeld, errors.Join(cause, err))
	}
	if err := check(ctx); err != nil {
		return held(err)
	}
	swarm, err := m.server.store.GetSwarm(ctx, actor.SwarmID)
	if err != nil {
		return held(err)
	}
	if err := m.server.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
		return held(err)
	}
	out := actor
	err = m.server.store.WithTx(ctx, func(ctx context.Context) error {
		if err := check(ctx); err != nil {
			return err
		}
		var err error
		phase, diagnostic := "error", managedProviderError(cause)
		if notRequested(actor) {
			phase = actor.Phase
			diagnostic = store.ManagedUnsentError(actor.Unsent(), diagnostic)
		}
		out, err = m.saveObservation(ctx, actor, phase, "", diagnostic)
		return err
	})
	if err != nil {
		return held(err)
	}
	return out, fmt.Errorf("%w: %w", errEphemeralObservationHeld, cause)
}

// Только наблюдения: новые stage grants и освобождение physical reservation
// принадлежат отдельному controller, не generic managed reconciler.
func (m *managedProvider) observeEphemeralLifecycle(ctx context.Context, actor store.ManagedActor, history store.EphemeralChildLifecycle) (store.ManagedActor, error) {
	s := m.server
	intent, err := s.store.GetEphemeralChildIntent(ctx, history.IntentID)
	if err != nil {
		return actor, err
	}
	parent, err := s.store.GetManagedActor(ctx, intent.Parent.Principal.ActorUID)
	if err != nil {
		return actor, err
	}
	kind := history.Kind
	if ephemeralCleanupLifecycle(kind) {
		kind = "cleanup"
	}
	current, pool, authorize, err := s.ephemeralLifecyclePreflight(ctx, parent.DelegatorID, intent.Parent, intent.ID, kind)
	if err != nil {
		return actor, fmt.Errorf("%w: %w", errEphemeralObservationHeld, err)
	}
	if current != actor {
		return actor, store.ErrRevisionConflict
	}
	release, err := m.effects.acquire(ctx, parent.UID, actor.UID)
	if err != nil {
		return actor, err
	}
	defer release()
	check := func(ctx context.Context) error {
		current, err := s.store.GetManagedActor(ctx, actor.UID)
		if err != nil {
			return err
		}
		if current != actor {
			return store.ErrRevisionConflict
		}
		return s.store.CheckEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID, authorize)
	}
	if err := check(ctx); err != nil {
		return actor, fmt.Errorf("%w: %w", errEphemeralObservationHeld, err)
	}
	knownStop := stopNotRequested(actor)
	if startNotRequested(actor) || actor.Stage == "delete" || (actor.Stage == "done" && history.Kind != "start") {
		return actor, nil
	}
	if actor.Stage == "done" && actor.Phase == "error" && actor.Error == managedProviderError(proxmox.ErrTaskFailed) {
		return actor, nil
	}
	if actor.UPID == "" && !knownStop {
		return m.retainEphemeralObservationFailure(ctx, actor, check, proxmox.ErrUncertain)
	}
	client, err := m.client(pool)
	if err != nil {
		return m.retainEphemeralObservationFailure(ctx, actor, check, err)
	}
	failed := false
	if actor.Stage != "done" && actor.UPID != "" {
		if err := client.WaitTask(ctx, actor.UPID); err != nil {
			if errors.Is(err, proxmox.ErrTaskFailed) {
				failed = true
			} else if managedTaskPending(ctx, err) {
				return actor, errManagedTaskPending
			} else {
				return m.retainEphemeralObservationFailure(ctx, actor, check, err)
			}
		}
	}
	observed, err := managedObservedState(ctx, client, actor)
	if err != nil {
		return m.retainEphemeralObservationFailure(ctx, actor, check, err)
	}
	ready := false
	if !failed {
		switch actor.Stage {
		case "start", "done":
			if history.Kind != "start" || observed != "running" {
				return m.retainEphemeralObservationFailure(ctx, actor, check, proxmox.ErrUncertain)
			}
			ready = m.hub != nil && m.hub.Connected(actor.UID, actor.Incarnation) && m.probe(ctx, actor)
		case "stop":
			if knownStop && observed == "running" {
				return actor, nil
			}
			// Absence закрывается вместе с physical reservation только в finalizer.
			if observed == "absent" {
				return actor, nil
			}
			if observed != "stopped" {
				return m.retainEphemeralObservationFailure(ctx, actor, check, proxmox.ErrUncertain)
			}
		default:
			return actor, store.ErrSwarmChildConflict
		}
	}
	swarm, err := s.store.GetSwarm(ctx, actor.SwarmID)
	if err != nil {
		return actor, err
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
		return actor, fmt.Errorf("%w: %w", errEphemeralObservationHeld, err)
	}
	out := actor
	err = s.store.WithTx(ctx, func(ctx context.Context) error {
		if err := check(ctx); err != nil {
			return fmt.Errorf("%w: %w", errEphemeralObservationHeld, err)
		}
		var err error
		if failed {
			out, err = m.saveObservation(ctx, actor, "error", "", managedProviderError(proxmox.ErrTaskFailed))
			if err != nil {
				return err
			}
			out, err = s.store.ResolveManagedActorTaskFailure(ctx, out.UID, out.Revision, out.OperationID, out.Stage, out.UPID, observed)
			return err
		}
		if actor.Stage == "stop" {
			out, err = s.store.ConfirmManagedActorStage(ctx, actor.UID, actor.Revision, actor.OperationID, "stop", "stopped")
			return err
		}
		if actor.Stage != "done" {
			out, err = m.confirmStartRunning(ctx, actor)
			if err != nil {
				return err
			}
		}
		if !ready {
			out, err = m.waitReadiness(ctx, out)
			return err
		}
		if out.Phase != "running" || out.Error != "" {
			out, err = m.saveObservation(ctx, out, "running", "", "")
		}
		return err
	})
	if err != nil {
		return actor, err
	}
	if ready {
		m.readiness.Delete(actor.UID)
		m.verified.Store(actor.UID, actor.Incarnation)
	}
	return out, nil
}
