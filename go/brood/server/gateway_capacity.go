package server

import (
	"context"
	"errors"
	"fmt"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// SetProjectCapacity rechecks administrator activity and writes shard settings atomically.
func (g *Gateway) SetProjectCapacity(actorID, projectID string, capacity app.ShardCapacity) error {
	return g.store.WithTx(context.Background(), func(ctx context.Context) error {
		user, err := g.store.GetUser(ctx, actorID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (user.Disabled || !user.IsAdmin)) {
			return app.ErrNotAllowed
		}
		if err != nil {
			return err
		}
		stored := store.ProjectCapacity{MaxSwarms: capacity.MaxSwarms, MaxActors: capacity.MaxActors, MaxCPUMillicores: capacity.MaxCPUMillicores, MaxMemoryBytes: capacity.MaxMemoryBytes}
		if err := store.ValidateProjectCapacity(stored); err != nil {
			return fmt.Errorf("%w: %v", app.ErrInvalidCapacity, err)
		}
		err = g.store.SetProjectCapacity(ctx, projectID, stored)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: project not found", app.ErrNotFound)
		}
		return err
	})
}

// ProjectCapacity supplies persisted shard properties to the domain layer.
func (g *Gateway) ProjectCapacity(projectID string) (app.ShardCapacity, error) {
	capacity, err := g.store.GetProjectCapacity(context.Background(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		return app.ShardCapacity{}, fmt.Errorf("%w: project not found", app.ErrNotFound)
	}
	if err != nil {
		return app.ShardCapacity{}, err
	}
	return app.ShardCapacity{MaxSwarms: capacity.MaxSwarms, MaxActors: capacity.MaxActors, MaxCPUMillicores: capacity.MaxCPUMillicores, MaxMemoryBytes: capacity.MaxMemoryBytes}, nil
}
