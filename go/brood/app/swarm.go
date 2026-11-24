package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"brood.dev/go/brood/store"
)

// SwarmState is the state of a swarm: live or a tombstone.
type SwarmState string

const (
	// StateActive is a live swarm.
	StateActive SwarmState = "active"
	// StateDeleted is a deleted swarm; its row stays as a record.
	StateDeleted SwarmState = "deleted"
)

// Swarm is a group of agents of one project and the scope of their message
// bus. It is open to its owner and to an active administrator.
type Swarm struct {
	ID        string
	ProjectID string
	OwnerID   string
	Name      string
	State     SwarmState
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SwarmOf converts a store row. The store writes RFC 3339 timestamps.
func SwarmOf(row store.Swarm) Swarm {
	sw := Swarm{ID: row.ID, ProjectID: row.ProjectID, OwnerID: row.OwnerID, Name: row.Name, State: SwarmState(row.State)}
	if row.CreatedAt != nil {
		sw.CreatedAt, _ = time.Parse(time.RFC3339Nano, *row.CreatedAt)
	}
	if row.UpdatedAt != nil {
		sw.UpdatedAt, _ = time.Parse(time.RFC3339Nano, *row.UpdatedAt)
	}
	return sw
}

// SwarmsOf converts store rows, keeping their order.
func SwarmsOf(rows []store.Swarm) []Swarm {
	out := make([]Swarm, 0, len(rows))
	for _, row := range rows {
		out = append(out, SwarmOf(row))
	}
	return out
}

// SwarmByID reads a swarm, deleted ones included, without an access check;
// found is false for a missing swarm. The caller checks access with Can.
func (s *Service) SwarmByID(ctx context.Context, id string) (sw Swarm, found bool, err error) {
	row, err := s.store.GetSwarm(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return Swarm{}, false, nil
	}
	if err != nil {
		return Swarm{}, false, err
	}
	return SwarmOf(row), true, nil
}

// readSwarm читает рой; отсутствующий — ErrNotFound, удалённый тоже,
// если нужен живой.
func (s *Service) readSwarm(ctx context.Context, swarmID string, live bool) (store.Swarm, error) {
	sw, err := s.store.GetSwarm(ctx, swarmID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && live && sw.State == string(StateDeleted)) {
		return store.Swarm{}, fmt.Errorf("%w: swarm not found", ErrNotFound)
	}
	return sw, err
}

// requireOwner пропускает к рою его владельца и активного администратора.
func (s *Service) requireOwner(ctx context.Context, userID string, sw store.Swarm) error {
	user, err := s.activeUser(ctx, userID)
	if err != nil {
		return err
	}
	if !user.IsAdmin && sw.OwnerID != userID {
		return ErrNotAllowed
	}
	return nil
}
