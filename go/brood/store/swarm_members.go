package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrLastSwarmOwner refuses removing or demoting the final owner.
var ErrLastSwarmOwner = errors.New("swarm must retain at least one owner")

// ErrInvalidSwarmRole names the supported ACL role dictionary.
var ErrInvalidSwarmRole = errors.New("swarm role must be one of: owner, editor, viewer")

// SwarmMember is an explicit grant; project membership is a separate prerequisite.
type SwarmMember struct {
	SwarmID string
	UserID  string
	Role    string
}

// GetSwarmRole returns an explicit grant, not a role inferred from ownership.
func (s *Store) GetSwarmRole(ctx context.Context, swarmID, userID string) (string, error) {
	var role string
	err := s.conn(ctx).QueryRowContext(ctx,
		`SELECT role FROM swarm_members WHERE swarm_id = ? AND user_id = ?`, swarmID, userID).Scan(&role)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("%w: swarm member", ErrNotFound)
	}
	return role, err
}

// ListSwarmMembers returns grants ordered by user ID; an unknown swarm is an error.
func (s *Store) ListSwarmMembers(ctx context.Context, swarmID string) ([]SwarmMember, error) {
	if _, err := s.GetSwarm(ctx, swarmID); err != nil {
		return nil, err
	}
	rows, err := s.conn(ctx).QueryContext(ctx,
		`SELECT swarm_id, user_id, role FROM swarm_members WHERE swarm_id = ? ORDER BY user_id`, swarmID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []SwarmMember{}
	for rows.Next() {
		var member SwarmMember
		if err := rows.Scan(&member.SwarmID, &member.UserID, &member.Role); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

// SetSwarmMember atomically grants or changes a role without orphaning the swarm.
func (s *Store) SetSwarmMember(ctx context.Context, swarmID, userID, role string) error {
	if role != "owner" && role != "editor" && role != "viewer" {
		return ErrInvalidSwarmRole
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		swarm, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		previous, err := s.GetSwarmRole(ctx, swarmID, userID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if previous == "owner" && role != "owner" {
			if err := s.guardSwarmOwner(ctx, swarmID); err != nil {
				return err
			}
		}
		if _, err := s.conn(ctx).ExecContext(ctx, `
			INSERT INTO swarm_members (swarm_id, user_id, role) VALUES (?, ?, ?)
			ON CONFLICT(swarm_id, user_id) DO UPDATE SET role = excluded.role`, swarmID, userID, role); err != nil {
			return err
		}
		return s.refreshSwarmOwner(ctx, swarm)
	})
}

// RemoveSwarmMember revokes one explicit grant, preserving another owner.
func (s *Store) RemoveSwarmMember(ctx context.Context, swarmID, userID string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		swarm, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		role, err := s.GetSwarmRole(ctx, swarmID, userID)
		if err != nil {
			return err
		}
		if role == "owner" {
			if err := s.guardSwarmOwner(ctx, swarmID); err != nil {
				return err
			}
		}
		if _, err := s.conn(ctx).ExecContext(ctx,
			`DELETE FROM swarm_members WHERE swarm_id = ? AND user_id = ?`, swarmID, userID); err != nil {
			return err
		}
		return s.refreshSwarmOwner(ctx, swarm)
	})
}

func (s *Store) guardSwarmOwner(ctx context.Context, swarmID string) error {
	var count int
	if err := s.conn(ctx).QueryRowContext(ctx,
		`SELECT count(*) FROM swarm_members WHERE swarm_id = ? AND role = 'owner'`, swarmID).Scan(&count); err != nil {
		return err
	}
	if count <= 1 {
		return ErrLastSwarmOwner
	}
	return nil
}

// refreshSwarmOwner maintains the legacy primary-owner projection for existing clients.
func (s *Store) refreshSwarmOwner(ctx context.Context, swarm Swarm) error {
	role, err := s.GetSwarmRole(ctx, swarm.ID, swarm.OwnerID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if role == "owner" {
		return nil
	}
	var ownerID string
	err = s.conn(ctx).QueryRowContext(ctx,
		`SELECT user_id FROM swarm_members WHERE swarm_id = ? AND role = 'owner' ORDER BY user_id LIMIT 1`, swarm.ID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		return ErrLastSwarmOwner
	}
	if err != nil {
		return err
	}
	return s.updateSwarm(ctx, swarm.ID, ownerID, `owner_id = ?, updated_at = ?`)
}
