package store

import (
	"context"
	"errors"
)

// ErrProjectHasSwarms protects bindings to native resources from a local cascade.
var ErrProjectHasSwarms = errors.New("project still has swarms; delete them first")

// DeleteProjectWithoutSwarms only deletes empty or tombstone-only projects.
// The check and cascade share a write transaction with concurrent swarm creation.
func (s *Store) DeleteProjectWithoutSwarms(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetProject(ctx, id); err != nil {
			return err
		}
		var live bool
		if err := s.conn(ctx).QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM swarms WHERE project_id = ? AND state <> 'deleted')`, id).Scan(&live); err != nil {
			return err
		}
		if live {
			return ErrProjectHasSwarms
		}
		return s.DeleteProject(ctx, id)
	})
}
