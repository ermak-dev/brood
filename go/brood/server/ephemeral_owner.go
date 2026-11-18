package server

import (
	"context"
	"errors"

	"brood.dev/go/brood/store"
)

// Это свидетельство принятого ручного intent, не новый scheduling grant.
func (s *Server) ownerActorOperationOrigin(ctx context.Context, actor store.ManagedActor) (*store.ActorOperation, error) {
	origin, err := s.store.GetOwnerManagedActorOperation(ctx, actor.UID, actor.Incarnation, actor.OperationID)
	if err == nil {
		kind := origin.Operation
		if kind == "update" {
			kind = "start"
		}
		if kind != actor.Operation {
			return nil, store.ErrSwarmChildDenied
		}
	} else if errors.Is(err, store.ErrNotFound) {
		link, err := s.store.GetDerivedOwnerManagedActorOperation(ctx, actor.UID, actor.Incarnation, actor.OperationID)
		if err != nil {
			return nil, err
		}
		if link.Operation != actor.Operation {
			return nil, store.ErrSwarmChildDenied
		}
		origin = link.Origin
	} else {
		return nil, err
	}
	swarm, err := s.store.GetSwarm(ctx, actor.SwarmID)
	if err != nil {
		return nil, err
	}
	if !origin.Owner || origin.State != "completed" || origin.UID != actor.UID || origin.SwarmID != actor.SwarmID || origin.ProjectID != swarm.ProjectID || swarm.State != "active" || swarm.AtespaceUID == nil || *swarm.AtespaceUID != actor.AtespaceUID {
		return nil, store.ErrSwarmChildDenied
	}
	return &origin, nil
}

func (m *managedProvider) continueOwnerResetStart(ctx context.Context, a store.ManagedActor, operationID string, link store.OwnerManagedActorLineage) (store.ManagedActor, error) {
	swarm, err := m.server.store.GetSwarm(ctx, a.SwarmID)
	if err != nil {
		return a, err
	}
	if err := m.server.checkSwarmNamespace(ctx, toAppSwarm(swarm)); err != nil {
		return a, err
	}
	var saved store.ManagedActor
	err = m.server.store.WithTx(ctx, func(tx context.Context) error {
		var err error
		saved, err = m.server.store.ContinueOwnerManagedActorReset(tx, a, operationID, func(tx context.Context, current store.OwnerManagedActorLineage) error {
			if current != link {
				return store.ErrSwarmChildDenied
			}
			live, err := m.server.store.GetSwarm(tx, swarm.ID)
			if err != nil {
				return err
			}
			if live.ProjectID != swarm.ProjectID || live.AtespaceName != swarm.AtespaceName || live.AtespaceUID == nil || swarm.AtespaceUID == nil || *live.AtespaceUID != *swarm.AtespaceUID {
				return store.ErrSwarmChildDenied
			}
			return nil
		})
		if err != nil {
			return err
		}
		saved, err = m.saveObservation(tx, saved, "stopped", "", "")
		return err
	})
	if err != nil {
		return a, err
	}
	return saved, nil
}

func (s *Server) ephemeralManualBootstrapOrigin(ctx context.Context, g guestRuntime) (*store.ActorOperation, error) {
	var origin *store.ActorOperation
	err := s.store.WithTx(ctx, func(ctx context.Context) error {
		actor, err := s.store.GetManagedActor(ctx, g.uid)
		if err != nil {
			return err
		}
		if actor.Class != store.ActorClassEphemeral || actor.Role != "worker" || actor.Incarnation != g.generation || actor.Operation != "start" || actor.Desired != "running" || actor.Phase == "deleted" {
			return store.ErrSwarmChildDenied
		}
		// Ответ Create/Start и readiness могут обновиться во время bootstrap read,
		// но новое intent или изменившаяся личность не наследуют старую авторизацию.
		snapshot := actor
		snapshot.Phase = g.actor.Phase
		snapshot.UPID = g.actor.UPID
		snapshot.Error = g.actor.Error
		snapshot.Revision = g.actor.Revision
		snapshot.UpdatedAt = g.actor.UpdatedAt
		snapshot.Stage = g.actor.Stage
		if snapshot != g.actor {
			return store.ErrSwarmChildDenied
		}
		origin, err = s.ownerActorOperationOrigin(ctx, actor)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if origin.ProjectID != g.swarm.ProjectID {
			return store.ErrSwarmChildDenied
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return origin, nil
}
