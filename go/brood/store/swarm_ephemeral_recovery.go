package store

import "context"

// AllocatedEphemeralChildIntents reads at most 128 retained jobs in insertion
// order for lifecycle recovery. Only the allocated live inventory incarnation is
// selected. This read grants no authority: every transition must recheck current
// parent, source, pool, manual interruption and its own operation provenance.
func (s *Store) AllocatedEphemeralChildIntents(ctx context.Context, after int64) ([]EphemeralChildIntent, error) {
	if after < 0 {
		return nil, ErrSwarmChildConflict
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT rowid,`+ephemeralChildIntentCols+` FROM ephemeral_child_intents WHERE rowid>? AND EXISTS(SELECT 1 FROM ephemeral_child_allocations a JOIN managed_actors m ON m.uid=a.actor_uid AND m.incarnation=a.actor_incarnation WHERE a.intent_id=ephemeral_child_intents.id AND m.phase<>'deleted') ORDER BY rowid LIMIT 128`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []EphemeralChildIntent{}
	for rows.Next() {
		intent, err := scanEphemeralChildIntent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
