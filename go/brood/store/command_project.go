package store

import (
	"context"
	"fmt"
)

// SetProjectActorLimit patches only actor admission, preserving all other shard properties.
// Authorization and the partial upsert share the same write transaction.
func (s *Store) SetProjectActorLimit(ctx context.Context, actorID, projectID string, actors *int64) error {
	if actors != nil && *actors < 0 {
		return fmt.Errorf("actor limit must be nonnegative")
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.GetUser(ctx, actorID)
		if err != nil {
			return err
		}
		if u.Disabled || !u.IsAdmin {
			return fmt.Errorf("administrator access required")
		}
		if _, err := s.GetProject(ctx, projectID); err != nil {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO project_capacity(project_id,worker_selector,max_actors) VALUES (?, '{}', ?) ON CONFLICT(project_id) DO UPDATE SET max_actors=excluded.max_actors`, projectID, actors)
		return err
	})
}

// CreateAdminProject creates a Brood inventory project, not a source checkout or build.
func (s *Store) CreateAdminProject(ctx context.Context, actorID, name, description string) (Project, error) {
	p := Project{ID: NewID(), Name: name}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.GetUser(ctx, actorID)
		if err != nil {
			return err
		}
		if u.Disabled || !u.IsAdmin {
			return fmt.Errorf("administrator access required")
		}
		if err := s.CreateProject(ctx, p); err != nil {
			return err
		}
		if description != "" {
			_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO command_project_details(project_id,description) VALUES (?,?)`, p.ID, description)
		}
		return err
	})
	return p, err
}
