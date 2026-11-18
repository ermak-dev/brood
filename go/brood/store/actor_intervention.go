package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"brood.dev/go/brood/pipeline"
)

func (s *Store) withInterventionTx(ctx context.Context, fn func(context.Context) error) error {
	return s.WithTx(ctx, func(ctx context.Context) (err error) {
		if _, err = s.conn(ctx).ExecContext(ctx, `SAVEPOINT owner_intervention`); err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err != nil {
				_, rollback := s.conn(cleanup).ExecContext(cleanup, `ROLLBACK TO owner_intervention`)
				err = errors.Join(err, rollback)
			}
			_, release := s.conn(cleanup).ExecContext(cleanup, `RELEASE owner_intervention`)
			err = errors.Join(err, release)
		}()
		return fn(ctx)
	})
}

func (s *Store) actorOwner(ctx context.Context, uid, human string) error {
	var allowed bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_actors a JOIN swarms sw ON sw.id=a.swarm_id JOIN users u ON u.id=? WHERE a.uid=? AND sw.state='active' AND u.disabled=0 AND (u.is_admin=1 OR (EXISTS(SELECT 1 FROM project_members m WHERE m.project_id=sw.project_id AND m.user_id=u.id) AND EXISTS(SELECT 1 FROM swarm_members m WHERE m.swarm_id=sw.id AND m.user_id=u.id AND m.role='owner'))))`, human, uid).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrSwarmChildDenied
	}
	return nil
}

// AcquireOwnerActorOperation journals explicit owner/admin intervention before
// effects, bypassing only automatic coordination holds, never identity fences.
// authorize must check the live authenticated session locally using ctx. A
// repeated ID never grants a second effect attempt, including after completion.
func (s *Store) AcquireOwnerActorOperation(ctx context.Context, target ManagedActor, id, caller, operation, human string, authorize func(context.Context) error) (ActorOperation, error) {
	var op ActorOperation
	if id == "" || caller == "" || len(caller) > 512 || human == "" || authorize == nil || !(sessionMutationOperation(operation) || operation == "restart" || operation == "wipe" || operation == "recycle" || operation == "stop" || operation == "reset" || operation == "delete" || operation == "update" || operation == "start") {
		return op, ErrSwarmChildDenied
	}
	err := s.withInterventionTx(ctx, func(ctx context.Context) error {
		if err := authorize(ctx); err != nil {
			return err
		}
		if err := s.actorOwner(ctx, target.UID, human); err != nil {
			return err
		}
		a, err := s.GetManagedActor(ctx, target.UID)
		if err != nil {
			return err
		}
		if a.Incarnation != target.Incarnation || a.SwarmID != target.SwarmID || a.AtespaceUID != target.AtespaceUID || a.Phase == "deleted" {
			return ErrRevisionConflict
		}
		if (sessionMutationOperation(operation) || operation == "restart" || operation == "wipe" || operation == "recycle") && (a.Desired != "running" || a.Phase != "running" || a.Stage != "done" || a.Operation != "start") {
			return newCoordinationConflict("actor incarnation is not ready for session control")
		}
		sw, err := s.GetSwarm(ctx, a.SwarmID)
		if err != nil {
			return err
		}
		if sw.AtespaceUID == nil || *sw.AtespaceUID != a.AtespaceUID {
			return ErrSwarmChildDenied
		}
		var exists bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_interventions WHERE id=?)`, id).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return newCoordinationConflict("owner intervention already recorded; inspect its outcome instead of replaying the effect")
		}
		op = ActorOperation{ProjectID: sw.ProjectID, SwarmID: sw.ID, UID: a.UID, Generation: a.Incarnation, OperationID: id, Caller: caller, Operation: operation, State: "inflight", Owner: true}
		if _, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_interventions(id,project_id,swarm_id,actor_uid,incarnation,human_id,caller,operation,state,created_at) VALUES(?,?,?,?,?,?,?,?,'inflight',?)`, id, sw.ProjectID, sw.ID, a.UID, a.Incarnation, human, caller, operation, nowRFC3339()); err != nil {
			return err
		}
		// Поздний ответ старого workspace RPC не должен отпустить эффект,
		// уже прерванный owner. Исходный slot и первый intervention ID остаются.
		if _, err = s.conn(ctx).ExecContext(ctx, `UPDATE coordination_claims SET payload=json_set(payload,'$.State','unconfirmed','$.InterruptedBy',COALESCE(NULLIF(json_extract(payload,'$.InterruptedBy'),''),?)) WHERE kind='workspace' AND slot=?`, id, a.UID); err != nil {
			return err
		}
		if err := s.interruptPendingActorStep(ctx, sw, a, id); err != nil {
			return err
		}
		// Запоминаем каждый затронутый текущий шаг, в том числе родителя child.
		if _, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_step_interruptions(intervention_id,swarm_id,sequence)
 SELECT ?,p.swarm_id,p.sequence FROM swarm_steps p WHERE p.state IN ('pending','attesting','delivering','inflight','unconfirmed','waiting') AND (p.actor_uid=? AND p.actor_incarnation=? OR EXISTS(SELECT 1 FROM swarm_child_joins j JOIN swarm_child_tasks c ON c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence WHERE j.swarm_id=p.swarm_id AND j.parent_sequence=p.sequence AND c.actor_uid=? AND c.actor_incarnation=? AND c.state IN ('pending','running')))`, id, a.UID, a.Incarnation, a.UID, a.Incarnation); err != nil {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='unconfirmed',detail='Explicit owner intervention interrupted execution; effects require reconciliation.' WHERE EXISTS(SELECT 1 FROM actor_step_interruptions i WHERE i.intervention_id=? AND i.swarm_id=swarm_steps.swarm_id AND i.sequence=swarm_steps.sequence)`, id)
		return err
	})
	return op, err
}

func (s *Store) interruptPendingActorStep(ctx context.Context, sw Swarm, a ManagedActor, intervention string) error {
	var sequence int64
	var nodeID string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,node_id FROM swarm_steps WHERE swarm_id=? AND sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?) AND state='pending' AND actor_uid IS NULL`, sw.ID, sw.ID).Scan(&sequence, &nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	doc, err := pipeline.Parse(string(sw.Graph), "pipeline")
	if err != nil {
		return err
	}
	node, ok := doc.Nodes[nodeID]
	if !ok || node.Type != "actor" {
		return nil
	}
	// До резерва у шага ещё нет actor_uid: его назначение задаёт binding роли.
	_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_step_interruptions(intervention_id,swarm_id,sequence) SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM swarm_role_bindings WHERE swarm_id=? AND role=? AND actor_uid=? AND actor_incarnation=?)`, intervention, sw.ID, sequence, sw.ID, node.Role, a.UID, a.Incarnation)
	return err
}

func (s *Store) currentOwnerActorOperation(ctx context.Context, uid string) (ActorOperation, error) {
	var op ActorOperation
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT project_id,swarm_id,actor_uid,incarnation,id,caller,operation,state FROM actor_interventions WHERE actor_uid=? AND state IN ('inflight','unconfirmed') ORDER BY rowid LIMIT 1`, uid).Scan(&op.ProjectID, &op.SwarmID, &op.UID, &op.Generation, &op.OperationID, &op.Caller, &op.Operation, &op.State)
	if errors.Is(err, sql.ErrNoRows) {
		return op, ErrNotFound
	}
	op.Owner = true
	return op, err
}

func (s *Store) finishOwnerActorOperation(ctx context.Context, op ActorOperation, confirmed bool) error {
	state := "unconfirmed"
	if confirmed {
		state = "completed"
	}
	result, err := s.conn(ctx).ExecContext(ctx, `UPDATE actor_interventions SET state=? WHERE id=? AND actor_uid=? AND incarnation=? AND caller=? AND operation=? AND state='inflight'`, state, op.OperationID, op.UID, op.Generation, op.Caller, op.Operation)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return newCoordinationConflict("owner intervention already settled or ownership changed")
	}
	return nil
}

// BeginOwnerManagedActorOperation commits explicit lifecycle intent and the
// interrupted execution evidence together. Provider task/ownership fences remain.
func (s *Store) BeginOwnerManagedActorOperation(ctx context.Context, target ManagedActor, id, operation, human string, authorize func(context.Context) error) (ManagedActor, error) {
	var result ManagedActor
	err := s.withInterventionTx(ctx, func(ctx context.Context) error {
		op, err := s.AcquireOwnerActorOperation(ctx, target, id, "operator:"+human, operation, human, authorize)
		if err != nil {
			return err
		}
		result, err = s.beginManagedActorOperation(ctx, target.UID, target.Revision, id, operation, true)
		if err != nil {
			return err
		}
		return s.finishOwnerActorOperation(ctx, op, true)
	})
	return result, err
}

func (s *Store) guardOwnerIntervention(ctx context.Context, uid string) error {
	var held bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_interventions WHERE actor_uid=? AND state IN ('inflight','unconfirmed')) OR EXISTS(SELECT 1 FROM actor_step_interruptions i JOIN swarm_steps p ON p.swarm_id=i.swarm_id AND p.sequence=i.sequence WHERE p.actor_uid=? OR EXISTS(SELECT 1 FROM swarm_child_tasks c JOIN swarm_child_joins j ON j.swarm_id=c.swarm_id AND j.parent_sequence=c.parent_sequence WHERE c.swarm_id=p.swarm_id AND c.parent_sequence=p.sequence AND c.actor_uid=? AND j.state!='aborted'))`, uid, uid, uid).Scan(&held)
	if err != nil {
		return err
	}
	if held {
		return newCoordinationConflict("owner intervention interrupted execution; automatic work requires explicit reconciliation")
	}
	return nil
}

func (s *Store) interruptedStep(ctx context.Context, swarm string, sequence int64) (bool, error) {
	var interrupted bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_step_interruptions WHERE swarm_id=? AND sequence=?)`, swarm, sequence).Scan(&interrupted)
	return interrupted, err
}
