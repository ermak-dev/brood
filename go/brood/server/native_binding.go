package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/substrate"
)

func (s *Server) checkSwarmNamespace(ctx context.Context, swarm app.Swarm) error {
	if swarm.AtespaceUID == "" || swarm.AtespaceName == "" {
		return fmt.Errorf("%w: swarm namespace identity is missing", app.ErrConflict)
	}
	if s.ateapi != nil {
		return checkNativeNamespace(ctx, s.ateapi, swarm)
	}
	if s.provision == nil {
		return &substrate.APIError{Status: 503, Detail: errAteapiNotConfiguredText}
	}
	uid, err := s.provision.GetAtespace(swarm.AtespaceName)
	if err != nil {
		return err
	}
	if uid != swarm.AtespaceUID {
		return fmt.Errorf("%w: swarm namespace identity changed", app.ErrConflict)
	}
	return nil
}

func checkNativeNamespace(ctx context.Context, client *substrate.Client, swarm app.Swarm) error {
	if swarm.AtespaceUID == "" || swarm.AtespaceName == "" {
		return fmt.Errorf("%w: swarm namespace identity is missing", app.ErrConflict)
	}
	space, err := client.GetAtespace(ctx, swarm.AtespaceName)
	if err != nil {
		return err
	}
	if space.GetMetadata().GetUid() != swarm.AtespaceUID || space.GetMetadata().GetName() != swarm.AtespaceName {
		return fmt.Errorf("%w: swarm namespace identity changed", app.ErrConflict)
	}
	if space.GetMetadata().GetDeleteTime() != nil {
		return fmt.Errorf("%w: swarm namespace is terminating", app.ErrConflict)
	}
	return nil
}

func (s *Server) nativeReadBound(w http.ResponseWriter, r *http.Request, swarm app.Swarm) bool {
	if err := s.checkSwarmNamespace(r.Context(), swarm); err != nil {
		apiError(w, r, err)
		return false
	}
	return true
}

func (s *Server) canUseNative(userID string, action app.Action, resource app.Resource) (bool, error) {
	allowed, err := s.app.Can(userID, action, resource)
	if err != nil || !allowed {
		return false, err
	}
	if resource.Kind != app.KindSwarm {
		return false, app.ErrNotAllowed
	}
	swarm, found, err := s.gateway.SwarmByID(resource.ID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, app.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.checkSwarmNamespace(ctx, swarm); err != nil {
		return false, err
	}
	return true, nil
}
