package server

import (
	"context"

	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/store"
)

// originRefusal — отказ создания актора из-за origin (422). Причина
// называет правило, но не повторяет origin: он мог содержать userinfo.
type originRefusal struct{ detail string }

func (e *originRefusal) Error() string { return "actor origin refused: " + e.detail }

// resolveActorOrigin выбирает origin нового актора: явный (канонизированный),
// иначе origin создающего master, иначе repository проекта, иначе пусто.
func (s *Server) resolveActorOrigin(ctx context.Context, projectID, requested string, parent *store.ManagedActor) (string, error) {
	if requested != "" {
		origin, err := gitorigin.FromRemote(requested)
		if err != nil {
			return "", &originRefusal{detail: err.Error()}
		}
		return origin, nil
	}
	if parent != nil && parent.Origin != "" {
		return parent.Origin, nil
	}
	return s.store.ProjectRepository(ctx, projectID)
}
