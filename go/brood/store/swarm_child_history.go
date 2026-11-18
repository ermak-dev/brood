package store

import "context"

// SwarmChildGroupSummary contains bounded metadata, never task payloads or reports.
// Open includes missing members; Interrupted is independent of task readiness.
type SwarmChildGroupSummary struct {
	Parent      SwarmChildParent
	Policy      SwarmChildJoinPolicy
	State       string
	Revision    int64
	Interrupted bool
	Registered  int
	Open        int
	Started     int
}

// SwarmChildGroupPage lists newer groups first. NextBefore is zero at the end.
type SwarmChildGroupPage struct {
	Items      []SwarmChildGroupSummary
	NextBefore int64
}

// GetSwarmChildHistory reads one historical group in a consistent snapshot.
// Trusted human adapters must authorize the swarm before exposing this data.
// This does not grant guest read, scheduling, or recovery authority.
func (s *Store) GetSwarmChildHistory(ctx context.Context, swarmID string, sequence int64) (SwarmChildJoin, error) {
	var result SwarmChildJoin
	if !validPipelineField(swarmID) || sequence < 1 {
		return result, ErrNotFound
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		parent, err := s.SwarmChildParentAt(ctx, swarmID, sequence)
		if err != nil {
			return err
		}
		result, err = s.loadChildJoin(ctx, parent)
		return err
	})
	return result, err
}

// SwarmChildGroups reads at most 100 metadata records in stable sequence order.
// A zero before cursor selects the newest page. Like SwarmSteps, this is an
// internal read: the caller must enforce current human swarm access.
func (s *Store) SwarmChildGroups(ctx context.Context, swarmID string, before int64, limit int) (SwarmChildGroupPage, error) {
	result := SwarmChildGroupPage{Items: []SwarmChildGroupSummary{}}
	if !validPipelineField(swarmID) || before < 0 || limit < 1 || limit > 100 {
		return result, ErrSwarmChildConflict
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT
 j.parent_sequence,j.pass,j.project_id,j.atespace_uid,j.parent_uid,j.parent_incarnation,j.parent_request_id,
 j.mode,j.total,j.quorum,j.remaining,j.state,j.revision,
 EXISTS(SELECT 1 FROM actor_step_interruptions i WHERE i.swarm_id=j.swarm_id AND i.sequence=j.parent_sequence),
 (SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence),
 (SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence AND c.state IN ('pending','running')),
 (SELECT count(*) FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence AND c.state IN ('running','completed'))
 FROM swarm_child_joins j WHERE j.swarm_id=? AND (?=0 OR j.parent_sequence<?)
 ORDER BY j.parent_sequence DESC LIMIT ?`, swarmID, before, before, limit+1)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var item SwarmChildGroupSummary
		var unsettled int
		item.Parent.Principal.SwarmID = swarmID
		err := rows.Scan(&item.Parent.Sequence, &item.Parent.Pass, &item.Parent.Principal.ProjectID, &item.Parent.Principal.AtespaceUID, &item.Parent.Principal.ActorUID, &item.Parent.Principal.Incarnation, &item.Parent.RequestID,
			&item.Policy.Mode, &item.Policy.Total, &item.Policy.Quorum, &item.Policy.Remaining, &item.State, &item.Revision, &item.Interrupted, &item.Registered, &unsettled, &item.Started)
		if err != nil {
			return result, err
		}
		if item.Registered > item.Policy.Total {
			return result, ErrSwarmChildConflict
		}
		if item.State != "aborted" {
			item.Open = item.Policy.Total - item.Registered + unsettled
		}
		result.Items = append(result.Items, item)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(result.Items) > limit {
		result.NextBefore = result.Items[limit-1].Parent.Sequence
		result.Items = result.Items[:limit]
	}
	return result, nil
}
