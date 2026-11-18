package store

import "context"

func (s *Store) interruptChildrenForPassStop(ctx context.Context, sw Swarm, userID string) error {
	var allowed bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users u WHERE u.id=? AND u.disabled=0 AND (u.is_admin=1 OR (EXISTS(SELECT 1 FROM project_members pm WHERE pm.project_id=? AND pm.user_id=u.id) AND EXISTS(SELECT 1 FROM swarm_members sm WHERE sm.swarm_id=? AND sm.user_id=u.id AND sm.role='owner'))))`, userID, sw.ProjectID, sw.ID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrSwarmRequestDenied
	}
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT parent_uid,parent_incarnation,parent_sequence FROM swarm_child_joins j WHERE j.swarm_id=? AND `+swarmChildUnfinished, sw.ID)
	if err != nil {
		return err
	}
	type parentAttempt struct {
		uid         string
		incarnation int64
		sequence    int64
	}
	var parents []parentAttempt
	for rows.Next() {
		var p parentAttempt
		if err := rows.Scan(&p.uid, &p.incarnation, &p.sequence); err != nil {
			rows.Close()
			return err
		}
		parents = append(parents, p)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, p := range parents {
		id := NewID()
		// completed описывает только зафиксированную остановку диспетчера,
		// а не остановку актора или отмену неизвестных дочерних эффектов.
		if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_interventions(id,project_id,swarm_id,actor_uid,incarnation,human_id,caller,operation,state,created_at) VALUES(?,?,?,?,?,?,?,'pass_stop','completed',?)`, id, sw.ProjectID, sw.ID, p.uid, p.incarnation, userID, "operator:"+userID, nowRFC3339()); err != nil {
			return err
		}
		if _, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_step_interruptions(intervention_id,swarm_id,sequence) VALUES(?,?,?)`, id, sw.ID, p.sequence); err != nil {
			return err
		}
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='unconfirmed',detail='The owner stopped this pass; child effects and reports remain retained for explicit reconciliation.' WHERE swarm_id=? AND sequence=?`, sw.ID, p.sequence); err != nil {
			return err
		}
	}
	return nil
}
