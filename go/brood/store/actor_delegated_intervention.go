package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"brood.dev/go/brood/pipeline"
)

// BeginDelegatedManagedActorOperation records an explicit command by a currently
// approved master to its own worker. The journal caller is the actor, not a human
// click; human is the live admin delegation anchor. authorize must recheck local
// request authority in this transaction, without network I/O. Automatic adapters
// must not use this entry point. Only automatic holds are bypassed.
func (s *Store) BeginDelegatedManagedActorOperation(ctx context.Context, source, target ManagedActor, id, operation, human string, authorize func(context.Context) error) (ManagedActor, error) {
	var result ManagedActor
	if authorize == nil || human == "" || source.UID == "" || !(operation == "start" || operation == "stop" || operation == "reset" || operation == "delete" || operation == "update") {
		return result, ErrSwarmChildDenied
	}
	err := s.withInterventionTx(ctx, func(ctx context.Context) error {
		caller := fmt.Sprintf("actor:%s:%d", source.UID, source.Incarnation)
		op, err := s.acquireExplicitActorOperation(ctx, target, id, caller, operation, human, "Explicit delegated actor intervention interrupted execution; effects require reconciliation.", func(ctx context.Context) error {
			if err := authorize(ctx); err != nil {
				return err
			}
			current, err := s.GetManagedActor(ctx, source.UID)
			if err != nil {
				return err
			}
			if current.Incarnation != source.Incarnation || current.SwarmID != source.SwarmID || current.AtespaceUID != source.AtespaceUID || current.Role != "master" || current.DelegatorID != human || current.DelegatorID != source.DelegatorID || current.Phase != "running" || current.Desired != "running" || current.Operation != "start" || current.Stage != "done" {
				return ErrSwarmChildDenied
			}
			user, err := s.GetUser(ctx, human)
			if err != nil {
				return err
			}
			if user.Disabled || !user.IsAdmin {
				return ErrSwarmChildDenied
			}
			worker, err := s.GetManagedActor(ctx, target.UID)
			if err != nil {
				return err
			}
			if worker.UID == current.UID || worker.ParentUID != current.UID || worker.Role != "worker" || worker.SwarmID != current.SwarmID || worker.AtespaceUID != current.AtespaceUID {
				return ErrSwarmChildDenied
			}
			// Старое automatic намерение нельзя объявить новым manual origin.
			if worker.OperationID == id {
				return ErrSwarmChildDenied
			}
			// Acquire проверит active swarm и его текущий namespace binding до журнала.
			return nil
		})
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

// Проверка durable postcondition ловит в том числе RAISE(IGNORE): SQL не вернул
// ошибку, но automatic evidence не было прервано. Savepoint откатит всё намерение.
func (s *Store) verifyExplicitActorInterruption(ctx context.Context, a ManagedActor, id string) error {
	var missing bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM coordination_claims WHERE kind='workspace' AND slot=? AND (COALESCE(json_extract(payload,'$.State'),'')!='unconfirmed' OR COALESCE(json_extract(payload,'$.InterruptedBy'),'')=''))
 OR EXISTS(SELECT 1 FROM swarm_steps p WHERE p.state IN ('pending','attesting','delivering','inflight','unconfirmed','waiting') AND (p.actor_uid=? AND p.actor_incarnation=? OR EXISTS(SELECT 1 FROM swarm_child_joins j JOIN swarm_child_tasks c ON c.swarm_id=j.swarm_id AND c.parent_sequence=j.parent_sequence WHERE j.swarm_id=p.swarm_id AND j.parent_sequence=p.sequence AND c.actor_uid=? AND c.actor_incarnation=? AND c.state IN ('pending','running')) OR EXISTS(SELECT 1 FROM ephemeral_child_allocations x JOIN ephemeral_child_intents e ON e.id=x.intent_id WHERE e.swarm_id=p.swarm_id AND e.parent_sequence=p.sequence AND e.parent_uid=p.actor_uid AND e.parent_incarnation=p.actor_incarnation AND e.parent_request_id=p.request_id AND x.actor_uid=? AND x.actor_incarnation=?)) AND (p.state!='unconfirmed' OR NOT EXISTS(SELECT 1 FROM actor_step_interruptions i WHERE i.intervention_id=? AND i.swarm_id=p.swarm_id AND i.sequence=p.sequence)))`, a.UID, a.UID, a.Incarnation, a.UID, a.Incarnation, a.UID, a.Incarnation, id).Scan(&missing)
	if err != nil {
		return err
	}
	if missing {
		return newCoordinationConflict("explicit actorinterruption evidence was not retained")
	}
	// Ещё не зарезервированный pending actor-шаг определяется ролью графа,
	// как в interruptPendingActorStep, а не отсутствующим actor_uid.
	var sequence int64
	var nodeID string
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,node_id FROM swarm_steps WHERE swarm_id=? AND sequence=(SELECT MAX(sequence) FROM swarm_steps WHERE swarm_id=?) AND actor_uid IS NULL AND state='pending'`, a.SwarmID, a.SwarmID).Scan(&sequence, &nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	sw, err := s.GetSwarm(ctx, a.SwarmID)
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
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM swarm_role_bindings WHERE swarm_id=? AND role=? AND actor_uid=? AND actor_incarnation=?) AND NOT EXISTS(SELECT 1 FROM actor_step_interruptions i JOIN swarm_steps p ON p.swarm_id=i.swarm_id AND p.sequence=i.sequence WHERE i.intervention_id=? AND i.swarm_id=? AND i.sequence=? AND p.state='unconfirmed')`, sw.ID, node.Role, a.UID, a.Incarnation, id, sw.ID, sequence).Scan(&missing)
	if err != nil {
		return err
	}
	if missing {
		return newCoordinationConflict("explicit actorpending-step interruption was not retained")
	}
	return nil
}
