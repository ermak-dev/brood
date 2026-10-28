package store

import (
	"context"
	"errors"
)

// ErrUserOwnsSwarms prevents deletion from orphaning infrastructure resources.
var ErrUserOwnsSwarms = errors.New("user still owns swarms; transfer or delete them first")

// UpdateUserProperties atomically replaces editable properties and revokes changed privileges.
func (s *Store) UpdateUserProperties(ctx context.Context, id, displayName string, admin, disabled bool) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		current, err := s.GetUser(ctx, id)
		if err != nil {
			return err
		}
		if !admin || disabled {
			if err := s.guardActiveAdmin(ctx, id); err != nil {
				return err
			}
		}
		if err := s.updateUser(ctx, id, `SET display_name = ?, is_admin = ?, disabled = ?, updated_at = ?`, displayName, admin, disabled, nowRFC3339()); err != nil {
			return err
		}
		if current.IsAdmin != admin || current.Disabled != disabled {
			_, err = s.DeleteSessionsOfUser(ctx, id)
			return err
		}
		return nil
	})
}

// DeleteUserWithoutSwarms removes an account only after its live swarms are gone.
func (s *Store) DeleteUserWithoutSwarms(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetUser(ctx, id); err != nil {
			return err
		}
		if err := s.guardActiveAdmin(ctx, id); err != nil {
			return err
		}
		var owns bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM swarms sw WHERE sw.state <> 'deleted' AND (
				sw.owner_id = ? OR EXISTS(SELECT 1 FROM swarm_members m
					WHERE m.swarm_id = sw.id AND m.user_id = ? AND m.role = 'owner'))
		)`, id, id).Scan(&owns); err != nil {
			return err
		}
		if owns {
			return ErrUserOwnsSwarms
		}
		return s.DeleteUser(ctx, id)
	})
}
