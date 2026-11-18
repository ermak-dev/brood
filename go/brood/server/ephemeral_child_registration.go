package server

import (
	"context"

	"brood.dev/go/brood/store"
)

func (s *Server) registerEphemeralChild(ctx context.Context, userID string, parent store.SwarmChildParent, intentID string) (store.SwarmChildTask, error) {
	intent, err := s.store.GetEphemeralChildIntent(ctx, intentID)
	if err != nil {
		return store.SwarmChildTask{}, err
	}
	if intent.Parent != parent || s.managed == nil {
		return store.SwarmChildTask{}, store.ErrSwarmChildDenied
	}
	return s.store.RegisterEphemeralChild(ctx, intentID, func(ctx context.Context, a store.SwarmChildAuthorization) error {
		if a.Action != "register_ephemeral" || a.Parent != parent {
			return store.ErrSwarmChildDenied
		}
		if err := s.authorizeSwarmOwnerIntent(ctx, userID, parent.Principal.SwarmID); err != nil {
			return err
		}
		return s.managed.childAuthority(ctx, a)
	})
}
