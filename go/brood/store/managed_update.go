package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"brood.dev/go/brood/pipeline"
)

// CheckActorUpdate reports, without writing, why an update of a would be
// refused now: a pipeline pass of its swarm, a step in progress on it or an
// active coordination claim involving it. It lets the server refuse before
// asking the guest; the intent repeats these checks in its own transaction.
func (s *Store) CheckActorUpdate(ctx context.Context, a ManagedActor) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.guardActorPipeline(ctx, a); err != nil {
			return err
		}
		return s.guardActorCoordination(ctx, a)
	})
}

// guardActorPipeline refuses an update (#133) while the actor's swarm runs a
// pipeline pass, running or waiting. A step in progress on the actor is named
// first (guardActorStep); any other pass is refused naming the swarm.
func (s *Store) guardActorPipeline(ctx context.Context, a ManagedActor) error {
	w, err := s.GetSwarm(ctx, a.SwarmID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// Inflight и unconfirmed шаги остановленного роя и прошлых проходов уже
	// не завершатся (StopSwarm), и держать ими актора вечно нельзя.
	if !activeExecution(w.Execution) {
		return nil
	}
	if w.Graph != nil {
		if err := s.guardActorStep(ctx, w, a); err != nil {
			return err
		}
	}
	// Шага у актора нет, но проход идёт: шаг, который станет pending для
	// роли актора во время перезапуска (ответ оператора на human-узел,
	// переход графа), диспетчер закрыл бы отказом «role has no live bound
	// actor», пока актор stopping или provisioning. Перезапуск занимает
	// загрузку набора и ожидание readiness, минуты. Проверка роли и
	// следующего узла заранее не видит переходов графа, поэтому держит весь
	// проход.
	return newCoordinationConflict(fmt.Sprintf("the pipeline pass of swarm %q is %s; update restarts actor %q, and a step that becomes pending for its role during the restart would be refused; stop the swarm or wait for the pass to end", w.Name, w.Execution, a.Name))
}

// guardActorStep refuses an update (#133) while the running or waiting swarm
// w runs a pipeline step on a: its latest step is pending for the role bound
// to this incarnation, or attesting, delivering, inflight or unconfirmed on
// it. The refusal names the step.
func (s *Store) guardActorStep(ctx context.Context, w Swarm, a ManagedActor) error {
	// Шаг в работе — только последний шаг идущего роя. Шаг прежней
	// инкарнации нынешний актор не продолжит.
	var sequence, incarnation int64
	var node, state, actor string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT sequence,node_id,state,COALESCE(actor_uid,''),COALESCE(actor_incarnation,0) FROM swarm_steps WHERE swarm_id=? ORDER BY sequence DESC LIMIT 1`, a.SwarmID).Scan(&sequence, &node, &state, &actor, &incarnation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch state {
	case "attesting", "delivering", "inflight", "unconfirmed":
		// Unconfirmed: исход подтверждения или доставки неизвестен, актор мог
		// получить инструкцию и работать над ней.
	case "pending":
		// Pending шаг ещё без актора, но диспетчер отдаст его привязанному к
		// роли узла; перезапуск сейчас закрыл бы шаг отказом.
		actor, incarnation, err = s.pendingStepActor(ctx, w, node)
		if err != nil {
			return err
		}
	default:
		return nil
	}
	if actor != a.UID || incarnation != a.Incarnation {
		return nil
	}
	return newCoordinationConflict(fmt.Sprintf("pipeline step %d (node %q) is %s for actor %q; update restarts the actor and would interrupt it; wait for the step to finish or stop the swarm", sequence, node, state, a.Name))
}

// pendingStepActor returns the actor bound to the role of actor node node of
// swarm w, or empty values for another node or an unbound role.
func (s *Store) pendingStepActor(ctx context.Context, w Swarm, node string) (string, int64, error) {
	d, err := pipeline.Parse(string(w.Graph), "pipeline")
	if err != nil {
		return "", 0, err
	}
	n, ok := d.Nodes[node]
	if !ok || n.Type != "actor" {
		return "", 0, nil
	}
	var uid string
	var incarnation int64
	err = s.conn(ctx).QueryRowContext(ctx, `SELECT actor_uid,actor_incarnation FROM swarm_role_bindings WHERE swarm_id=? AND role=?`, w.ID, n.Role).Scan(&uid, &incarnation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return uid, incarnation, err
}
