package server

import (
	"context"

	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/store"
)

// managedPublicHost: сеть воркера не ограничена, поэтому direct-режим
// допустим для любого хоста.
func managedPublicHost(string) bool { return true }

// originRefusal — отказ создания актора из-за origin (422). Причина
// называет правило, но не повторяет origin: он мог содержать userinfo.
type originRefusal struct{ detail string }

func (e *originRefusal) Error() string { return "actor origin refused: " + e.detail }

const originModeRefusal = "origin must be the project repository with server Git access (a project Git credential or BROOD_GIT_REMOTES) or an HTTPS repository the worker can reach directly"

// originMode пересчитывает режим доступа origin по текущему Git-доступу
// проекта. Пустой origin режима не имеет.
func (s *Server) originMode(ctx context.Context, projectID, origin string) (string, error) {
	if origin == "" {
		return "", nil
	}
	access, err := s.projectGitAccess(ctx, projectID)
	if err != nil {
		return "", err
	}
	return access.mode(origin), nil
}

// settledOriginMode is originMode for a response after a committed mutation:
// a failed read must not turn a successful write into an error, so it reports
// no mode and the next read recomputes it.
func (s *Server) settledOriginMode(ctx context.Context, projectID, origin string) string {
	mode, err := s.originMode(ctx, projectID, origin)
	if err != nil {
		return ""
	}
	return mode
}

func (s *Server) reserveManagedActorOrigin(ctx context.Context, request store.ManagedActor) (store.ManagedActor, error) {
	var actor store.ManagedActor
	err := s.store.WithTx(ctx, func(tx context.Context) error {
		var err error
		actor, err = s.store.ReserveManagedActor(tx, request)
		if err != nil {
			return err
		}
		swarm, err := s.store.GetSwarm(tx, actor.SwarmID)
		if err != nil {
			return err
		}
		if actor.Origin == "" {
			return nil
		}
		mode, err := s.originMode(tx, swarm.ProjectID, actor.Origin)
		if err != nil {
			return err
		}
		if mode == "" {
			return &originRefusal{detail: originModeRefusal}
		}
		return nil
	})
	return actor, err
}

// resolveActorOrigin выбирает origin нового актора: явный (канонизированный),
// иначе origin создающего master, иначе repository проекта, иначе пусто.
// Непустой origin без режима — отказ, а не тихий пустой workspace.
func (s *Server) resolveActorOrigin(ctx context.Context, projectID, requested string, parent *store.ManagedActor) (string, error) {
	access, err := s.projectGitAccess(ctx, projectID)
	if err != nil {
		return "", err
	}
	origin := access.repository
	switch {
	case requested != "":
		if origin, err = gitorigin.Canonical(requested); err != nil {
			return "", &originRefusal{detail: err.Error()}
		}
	case parent != nil && parent.Origin != "":
		origin = parent.Origin
	}
	if origin == "" {
		return "", nil
	}
	if access.mode(origin) == "" {
		return "", &originRefusal{detail: originModeRefusal}
	}
	return origin, nil
}
