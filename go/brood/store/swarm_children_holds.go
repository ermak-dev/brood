package store

import "context"

func (s *Store) hasUnfinishedSwarmChildren(ctx context.Context, swarmID string) (bool, error) {
	var unfinished bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM swarm_child_joins j WHERE j.swarm_id=? AND `+swarmChildUnfinished+`)`, swarmID).Scan(&unfinished)
	return unfinished, err
}

// CheckActorChildObligations refuses mutations of an actor held by durable child
// work. Session-control adapters must check this before mutating RPCs, including
// their final authorization check; read-only inspection does not need this gate.
func (s *Store) CheckActorChildObligations(ctx context.Context, uid string) error {
	return s.guardChildLifecycle(ctx, uid)
}

// guardChildLifecycle protects both executors and the parent, including missing
// expected members. Holds are durable and never expire into permission to reset.
func (s *Store) guardChildLifecycle(ctx context.Context, uid string) error {
	if err := s.guardOwnerIntervention(ctx, uid); err != nil {
		return err
	}
	var held bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM swarm_child_joins j WHERE
 (j.parent_uid=? AND `+swarmChildUnfinished+`) OR EXISTS(SELECT 1 FROM swarm_child_tasks c WHERE c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence AND c.actor_uid=? AND c.state IN ('pending','running')))`, uid, uid).Scan(&held)
	if err != nil {
		return err
	}
	if held {
		return newCoordinationConflict("outstanding child obligations hold this actor; cancel unsent children or reconcile running children before changing lifecycle or workspace")
	}
	return nil
}

// guardChildAllocation runs in the same write transaction as the start CAS.
// Queued children do not consume simultaneous execution capacity.
func (s *Store) guardChildAllocation(ctx context.Context, uid string) error {
	if err := s.guardOwnerIntervention(ctx, uid); err != nil {
		return err
	}
	var busy bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM swarm_child_tasks WHERE actor_uid=? AND state='running') OR
 EXISTS(SELECT 1 FROM coordination_claims WHERE slot IS NOT NULL AND (slot=? OR json_extract(payload,'$.Source.UID')=? OR json_extract(payload,'$.Target.UID')=?))`, uid, uid, uid, uid).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return newCoordinationConflict("child executor allocation is already in use")
	}
	a, err := s.GetManagedActor(ctx, uid)
	if err != nil {
		return err
	}
	w, err := s.GetSwarm(ctx, a.SwarmID)
	if err != nil {
		return err
	}
	// Как и lifecycle guard, считаем занятым только последний шаг идущего
	// роя на текущей инкарнации, включая pending для привязанной роли.
	if activeExecution(w.Execution) && w.Graph != nil {
		return s.guardActorStep(ctx, w, a)
	}
	return nil
}
