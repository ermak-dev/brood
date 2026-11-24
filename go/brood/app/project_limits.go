package app

import (
	"context"
	"errors"
	"fmt"

	"brood.dev/go/brood/store"
)

// ProjectLimits returns the limits of a project to a user who sees it.
func (s *Service) ProjectLimits(ctx context.Context, userID, projectID string) (store.ProjectCapacity, error) {
	if err := s.requireProjectView(ctx, userID, projectID); err != nil {
		return store.ProjectCapacity{}, err
	}
	return s.store.GetProjectCapacity(ctx, projectID)
}

// SetProjectLimits replaces the limits of a project for an active
// administrator, in turn with swarm creation and agent admission in the
// project. Lowering a limit keeps what is already held.
func (s *Service) SetProjectLimits(ctx context.Context, actorID, projectID string, limits store.ProjectCapacity) error {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return err
	}
	if err := s.projectExists(ctx, projectID); err != nil {
		return err
	}
	if err := store.ValidateProjectCapacity(limits); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCapacity, err)
	}
	unlock, err := s.lockProject(ctx, projectID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.store.WithTx(ctx, func(ctx context.Context) error {
		if err := s.requireAdmin(ctx, actorID); err != nil {
			return err
		}
		err := s.store.SetProjectCapacity(ctx, projectID, limits)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: project not found", ErrNotFound)
		}
		return err
	})
}
