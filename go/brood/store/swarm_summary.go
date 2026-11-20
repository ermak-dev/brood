package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// SwarmSummary is the number of live workers of a swarm.
type SwarmSummary struct {
	ManagedActorCount int64
}

// SwarmSummaries returns the summary of each named swarm in one query. A
// swarm missing from the database is missing from the map. The caller
// names only swarms it may show: the summary does not check access.
func (s *Store) SwarmSummaries(ctx context.Context, ids []string) (map[string]SwarmSummary, error) {
	out := make(map[string]SwarmSummary, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	set, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `
		SELECT s.id, (SELECT COUNT(*) FROM managed_actors a WHERE a.swarm_id = s.id AND a.phase <> 'deleted')
		FROM swarms s WHERE s.id IN (SELECT value FROM json_each(?))`, string(set))
	if err != nil {
		return nil, fmt.Errorf("swarm summaries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var summary SwarmSummary
		if err := rows.Scan(&id, &summary.ManagedActorCount); err != nil {
			return nil, fmt.Errorf("swarm summaries: %w", err)
		}
		out[id] = summary
	}
	return out, rows.Err()
}

// ProjectSwarmCounts counts, in one query, the swarms of each named project
// that the user sees: every live swarm for an active administrator, otherwise
// the swarms the user owns. A disabled or unknown user sees no swarm. Every
// named project is in the map, with zero if nothing is visible.
func (s *Store) ProjectSwarmCounts(ctx context.Context, userID string, projectIDs []string) (map[string]int64, error) {
	out := make(map[string]int64, len(projectIDs))
	if len(projectIDs) == 0 {
		return out, nil
	}
	set, err := json.Marshal(projectIDs)
	if err != nil {
		return nil, err
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `
		SELECT p.value, COUNT(s.id)
		FROM json_each(?) p
		LEFT JOIN swarms s ON s.project_id = p.value AND s.state <> 'deleted'
			AND EXISTS (SELECT 1 FROM users u WHERE u.id = ? AND u.disabled = 0
				AND (u.is_admin = 1 OR s.owner_id = u.id))
		GROUP BY p.value`, string(set), userID)
	if err != nil {
		return nil, fmt.Errorf("project swarm counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("project swarm counts: %w", err)
		}
		out[id] = count
	}
	return out, rows.Err()
}
