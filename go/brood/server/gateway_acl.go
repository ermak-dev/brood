package server

import (
	"context"
	"errors"
	"fmt"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// SwarmRole reads an explicit local grant, without inferring project membership.
func (g *Gateway) SwarmRole(swarmID, userID string) (string, bool, error) {
	role, err := g.store.GetSwarmRole(context.Background(), swarmID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	return role, err == nil, err
}

// SwarmMembers returns the local grants consumed by the domain layer.
func (g *Gateway) SwarmMembers(swarmID string) ([]app.SwarmMember, error) {
	members, err := g.store.ListSwarmMembers(context.Background(), swarmID)
	if err != nil {
		return nil, aclError(err)
	}
	out := make([]app.SwarmMember, 0, len(members))
	for _, member := range members {
		out = append(out, app.SwarmMember{SwarmID: swarmID, UserID: member.UserID, Role: member.Role})
	}
	return out, nil
}

// SwarmsByMember lists explicitly granted resources; membership is checked by Service.
func (g *Gateway) SwarmsByMember(projectID, userID string) ([]app.Swarm, error) {
	rows, err := g.catalog.Swarms(context.Background())
	if err != nil {
		return nil, err
	}
	out := []app.Swarm{}
	for _, swarm := range rows {
		if swarm.ProjectID != projectID {
			continue
		}
		_, granted, err := g.SwarmRole(swarm.ID, userID)
		if err != nil {
			return nil, err
		}
		if granted {
			out = append(out, toAppSwarmRow(swarm))
		}
	}
	return out, nil
}

// SetSwarmMember rechecks both actor and grantee while holding the write lock.
func (g *Gateway) SetSwarmMember(actorID, swarmID, userID, role string) error {
	return g.store.WithTx(context.Background(), func(ctx context.Context) error {
		swarm, err := g.authorizeACLMutation(ctx, actorID, swarmID)
		if err != nil {
			return err
		}
		if err := g.requireACLGrantee(ctx, swarm.ProjectID, userID); err != nil {
			return err
		}
		return aclError(g.store.SetSwarmMember(ctx, swarmID, userID, role))
	})
}

// RemoveSwarmMember revokes a grant without permitting a stale owner to write.
func (g *Gateway) RemoveSwarmMember(actorID, swarmID, userID string) error {
	return g.store.WithTx(context.Background(), func(ctx context.Context) error {
		if _, err := g.authorizeACLMutation(ctx, actorID, swarmID); err != nil {
			return err
		}
		return aclError(g.store.RemoveSwarmMember(ctx, swarmID, userID))
	})
}

func (g *Gateway) authorizeACLMutation(ctx context.Context, actorID, swarmID string) (store.Swarm, error) {
	swarm, err := g.store.GetSwarm(ctx, swarmID)
	if err != nil {
		return swarm, aclError(err)
	}
	actor, err := g.store.GetUser(ctx, actorID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && actor.Disabled) {
		return swarm, app.ErrNotAllowed
	}
	if err != nil {
		return swarm, err
	}
	if !actor.IsAdmin {
		member, err := g.store.IsMember(ctx, swarm.ProjectID, actorID)
		if err != nil {
			return swarm, err
		}
		if !member {
			return swarm, app.ErrNotAllowed
		}
		role, err := g.store.GetSwarmRole(ctx, swarmID, actorID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && role != app.RoleOwner) {
			return swarm, app.ErrNotAllowed
		}
		if err != nil {
			return swarm, err
		}
	}
	if swarm.State != string(app.StateActive) {
		return swarm, fmt.Errorf("%w: ACL change from %s", app.ErrState, swarm.State)
	}
	return swarm, nil
}

func (g *Gateway) requireACLGrantee(ctx context.Context, projectID, userID string) error {
	user, err := g.store.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && user.Disabled) {
		return app.ErrNotAllowed
	}
	if err != nil {
		return err
	}
	member, err := g.store.IsMember(ctx, projectID, userID)
	if err != nil {
		return err
	}
	if !member && !user.IsAdmin {
		return app.ErrNotAllowed
	}
	return nil
}

func aclError(err error) error {
	switch {
	case errors.Is(err, store.ErrLastSwarmOwner):
		return fmt.Errorf("%w: %s", app.ErrConflict, store.ErrLastSwarmOwner)
	case errors.Is(err, store.ErrInvalidSwarmRole):
		return app.ErrInvalidRole
	case errors.Is(err, store.ErrNotFound):
		return app.ErrNotFound
	default:
		return err
	}
}
