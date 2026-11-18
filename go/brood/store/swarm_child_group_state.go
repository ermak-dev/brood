package store

import (
	"context"
	"database/sql"
	"errors"
)

// SwarmChildGroupState reads retained state for a trusted server adapter.
// It grants no child scheduling or historical material access by itself.
func (s *Store) SwarmChildGroupState(ctx context.Context, swarmID string, sequence int64) (string, error) {
	var state string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT state FROM swarm_child_joins WHERE swarm_id=? AND parent_sequence=?`, swarmID, sequence).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return state, err
}
