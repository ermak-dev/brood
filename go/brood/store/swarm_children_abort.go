package store

import (
	"context"
	"time"
)

func (s *Store) childAbortIdentity(ctx context.Context, p SwarmChildParent, session string) (string, error) {
	sess, err := s.GetSession(ctx, session)
	if err != nil || (sess.ExpiresAt != nil && !sess.ExpiresAt.After(time.Now())) {
		return "", ErrSwarmChildDenied
	}
	var allowed bool
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN swarms sw ON sw.id=? AND sw.project_id=?
 WHERE u.id=? AND u.disabled=0 AND (u.is_admin=1 OR (
 EXISTS(SELECT 1 FROM project_members pm WHERE pm.project_id=sw.project_id AND pm.user_id=u.id) AND
 EXISTS(SELECT 1 FROM swarm_members sm WHERE sm.swarm_id=sw.id AND sm.user_id=u.id AND sm.role='owner'))))`, p.Principal.SwarmID, p.Principal.ProjectID, sess.UserID).Scan(&allowed)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", ErrSwarmChildDenied
	}
	return sess.UserID, nil
}

// AbortSwarmChildJoin releases only an incomplete, wholly unstarted group.
// It atomically cancels pending intent, retains cancelled children and the exact
// original policy, and journals the human abort. Any running/completed child
// rejects the entire operation: unknown effects are never cleared here.
// A current owner/admin web session and an explicit abort policy are mandatory,
// including on exact retries with the original join revision. Worker identity,
// scheduling authority and parent liveness are not recovery prerequisites.
// The policy runs in the write transaction; HumanID/SessionHash identify the
// authenticated human, Caller is empty, and Parent pins the historical group.
// No parent/graph transition or guest effect is performed.
func (s *Store) AbortSwarmChildJoin(ctx context.Context, p SwarmChildParent, session string, expected int64, authorize SwarmChildAuthorize) (SwarmChildJoin, error) {
	if expected < 1 {
		return SwarmChildJoin{}, ErrSwarmChildConflict
	}
	var result SwarmChildJoin
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if authorize == nil {
			return ErrSwarmChildDenied
		}
		join, err := s.loadChildJoin(ctx, p)
		if err != nil {
			return err
		}
		human, err := s.childAbortIdentity(ctx, p, session)
		if err != nil {
			return err
		}
		if err := authorize(ctx, SwarmChildAuthorization{Action: "abort", Parent: p, HumanID: human, SessionHash: session}); err != nil {
			return err
		}
		current, err := s.childAbortIdentity(ctx, p, session)
		if err != nil {
			return err
		}
		if current != human {
			return ErrSwarmChildDenied
		}
		// Даже callback не может подменить проверяемое состояние группы.
		join, err = s.loadChildJoin(ctx, p)
		if err != nil {
			return err
		}
		if join.State == "aborted" && join.Revision == expected+1 && join.AbortedBy == human {
			result = join
			return nil
		}
		if join.State != "open" || join.Revision != expected || len(join.Children) >= join.Policy.Total {
			return ErrSwarmChildConflict
		}
		for _, c := range join.Children {
			if c.State != "pending" && c.State != "cancelled" {
				return ErrSwarmChildConflict
			}
		}
		finished := nowRFC3339()
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE swarm_child_tasks SET state='cancelled',revision=revision+1,finished_at=? WHERE swarm_id=? AND parent_sequence=? AND state='pending'`, finished, p.Principal.SwarmID, p.Sequence)
		if err != nil {
			return err
		}
		update, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_child_joins SET state='aborted',revision=revision+1,aborted_by=?,aborted_at=? WHERE swarm_id=? AND parent_sequence=? AND state='open' AND revision=?`, human, finished, p.Principal.SwarmID, p.Sequence, expected)
		if err != nil {
			return err
		}
		n, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrSwarmChildConflict
		}
		result, err = s.loadChildJoin(ctx, p)
		return err
	})
	if err != nil {
		return SwarmChildJoin{}, err
	}
	return result, nil
}
