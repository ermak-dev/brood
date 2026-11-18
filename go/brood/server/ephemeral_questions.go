package server

import (
	"context"
	"errors"

	"brood.dev/go/brood/store"
)

func (m *managedProvider) openEphemeralOperatorRequest(ctx context.Context, actor store.ManagedActor, in store.ActorRequestInput) (store.SwarmRequest, error) {
	job, err := m.server.store.GetEphemeralChildIntentForActor(ctx, actor.UID, actor.Incarnation)
	if err != nil {
		return store.SwarmRequest{}, err
	}
	group, err := m.server.store.GetSwarmChildJoin(ctx, job.Parent, m.childAuthority)
	if err != nil {
		return store.SwarmRequest{}, err
	}
	for _, child := range group.Children {
		if child.RequestID == job.RequestID && child.Executor == childPrincipal(actor, job.Parent.Principal.ProjectID) {
			return m.server.store.OpenEphemeralChildRequest(ctx, child.ID, in, m.childAuthority)
		}
	}
	return store.SwarmRequest{}, store.ErrSwarmRequestDenied
}

// Allocation history keeps the source scoped after inventory removal. An
// ephemeral actor without that history must never acquire legacy transport.
func (s *Server) ephemeralQuestionJob(ctx context.Context, request store.SwarmRequest) (*store.EphemeralChildIntent, error) {
	job, err := s.store.GetEphemeralChildIntentForActor(ctx, request.ActorUID, request.ActorIncarnation)
	if err == nil {
		return &job, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	actor, err := s.store.GetManagedActor(ctx, request.ActorUID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if actor.Class == store.ActorClassEphemeral {
		return nil, store.ErrSwarmRequestDenied
	}
	return nil, nil
}

func (d *actorDeliveryService) checkEphemeralAnswerAuthority(ctx context.Context, intent store.ActorDeliveryIntent) error {
	if intent.Input.SourceKind != "answer" {
		return nil
	}
	q, err := d.server.store.GetSwarmRequest(ctx, intent.Input.SwarmID, intent.Input.SourceID)
	if err != nil {
		return err
	}
	job, err := d.server.ephemeralQuestionJob(ctx, q)
	if err != nil || job == nil {
		return err
	}
	if d.server.managed == nil {
		return store.ErrSwarmRequestDenied
	}
	return d.server.store.CheckEphemeralChildAnswer(ctx, q.SwarmID, q.ID, d.server.managed.childAuthority)
}

func (d *actorDeliveryService) deliveryEffectActors(ctx context.Context, intent store.ActorDeliveryIntent) ([]string, error) {
	actors := []string{intent.Input.ActorUID}
	if intent.Input.SourceKind == "child" {
		job, err := d.server.store.GetEphemeralChildIntentForTask(ctx, intent.Input.SourceID)
		if err != nil {
			return nil, err
		}
		return append(actors, job.Parent.Principal.ActorUID), nil
	}
	if intent.Input.SourceKind == "answer" {
		q, err := d.server.store.GetSwarmRequest(ctx, intent.Input.SwarmID, intent.Input.SourceID)
		if err != nil {
			return nil, err
		}
		job, err := d.server.ephemeralQuestionJob(ctx, q)
		if err != nil {
			return nil, err
		}
		if job != nil {
			actors = append(actors, job.Parent.Principal.ActorUID)
		}
	}
	return actors, nil
}
