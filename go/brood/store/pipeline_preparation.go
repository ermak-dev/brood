package store

import "context"

// Подготовка выделенного актора под узел закрыта (#208): узел называет роль,
// рой связывает её с существующим актором. Таблица pipeline_preparations и
// её триггеры остаются в схеме, пока она не сменится; записанное прежде
// намерение по-прежнему неизменно, а RecoverPipelinePreparations переводит
// его в unconfirmed.

// PipelinePreparationRequest identified a new dedicated actor and its
// workflow visit; it remains the argument of the closed entry point.
type PipelinePreparationRequest struct {
	SwarmID, RunID, NodeID, RequestID, UserID string
	Sequence                                  int64
	Actor                                     ManagedActor
}

// PipelinePreparation was an allocation intent, not permission to deliver
// work; the closed entry point never returns one.
type PipelinePreparation struct {
	ActorUID     string
	Incarnation  int64
	PoolUID      string
	PoolRevision int64
	Challenge    string
	Replay       bool
}

const pipelinePreparationActorFence = `CREATE TRIGGER pipeline_preparations_actor_fence BEFORE UPDATE ON managed_actors WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE actor_uid=OLD.uid AND state IN ('reserved','unconfirmed')) BEGIN SELECT RAISE(ABORT, 'pipeline preparation actor is not verified'); END`
const pipelinePreparationActorNoReplace = `CREATE TRIGGER pipeline_preparations_actor_no_replace BEFORE INSERT ON managed_actors WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE actor_uid=NEW.uid) BEGIN SELECT RAISE(ABORT, 'pipeline preparation actor is immutable'); END`
const pipelinePreparationNoDelete = `CREATE TRIGGER pipeline_preparations_no_delete BEFORE DELETE ON pipeline_preparations WHEN EXISTS (SELECT 1 FROM swarms WHERE id=OLD.swarm_id) BEGIN SELECT RAISE(ABORT, 'pipeline preparation is immutable'); END`
const pipelinePreparationNoReplace = `CREATE TRIGGER pipeline_preparations_no_replace BEFORE INSERT ON pipeline_preparations WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE (swarm_id=NEW.swarm_id AND (sequence=NEW.sequence OR request_id=NEW.request_id)) OR actor_uid=NEW.actor_uid OR challenge_digest=NEW.challenge_digest) BEGIN SELECT RAISE(ABORT, 'pipeline preparation is immutable'); END`
const pipelinePreparationUpdateFence = `CREATE TRIGGER pipeline_preparations_update_fence BEFORE UPDATE ON pipeline_preparations WHEN NEW.swarm_id IS NOT OLD.swarm_id OR NEW.sequence IS NOT OLD.sequence OR NEW.node_id IS NOT OLD.node_id OR NEW.request_id IS NOT OLD.request_id OR NEW.actor_uid IS NOT OLD.actor_uid OR NEW.actor_incarnation IS NOT OLD.actor_incarnation OR NEW.pool_uid IS NOT OLD.pool_uid OR NEW.pool_revision IS NOT OLD.pool_revision OR NEW.request_digest IS NOT OLD.request_digest OR NEW.challenge_digest IS NOT OLD.challenge_digest OR NEW.created_at IS NOT OLD.created_at OR (OLD.state='unconfirmed' AND NEW.state!='unconfirmed') BEGIN SELECT RAISE(ABORT, 'pipeline preparation is immutable'); END`
const pipelinePreparationStepNoReplace = `CREATE TRIGGER pipeline_preparations_step_no_replace BEFORE INSERT ON swarm_steps WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE swarm_id=NEW.swarm_id) BEGIN SELECT RAISE(ABORT, 'pipeline preparation visit is unresolved'); END`
const pipelinePreparationStepUpdateFence = `CREATE TRIGGER pipeline_preparations_step_update_fence BEFORE UPDATE ON swarm_steps WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE swarm_id=OLD.swarm_id AND sequence=OLD.sequence) AND (NEW.swarm_id IS NOT OLD.swarm_id OR NEW.sequence IS NOT OLD.sequence OR NEW.node_id IS NOT OLD.node_id OR NEW.actor_uid IS NOT OLD.actor_uid OR NEW.actor_incarnation IS NOT OLD.actor_incarnation OR NEW.request_id IS NOT OLD.request_id OR NEW.started_at IS NOT OLD.started_at OR NEW.outcome IS NOT OLD.outcome OR NEW.native_actor_name IS NOT OLD.native_actor_name OR NEW.native_template_name IS NOT OLD.native_template_name OR NEW.native_template_uid IS NOT OLD.native_template_uid OR NEW.native_template_digest IS NOT OLD.native_template_digest OR NOT ((OLD.state='preparing' AND NEW.state IN ('preparing','unconfirmed')) OR (OLD.state='unconfirmed' AND NEW.state='unconfirmed'))) BEGIN SELECT RAISE(ABORT, 'pipeline preparation visit is immutable'); END`
const pipelinePreparationSwarmNoDelete = `CREATE TRIGGER pipeline_preparations_swarm_no_delete BEFORE DELETE ON swarms WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE swarm_id=OLD.id) BEGIN SELECT RAISE(ABORT, 'pipeline preparation remains unresolved'); END`

const pipelinePreparationSchema = `CREATE TABLE pipeline_preparations (
    swarm_id TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    node_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    actor_uid TEXT NOT NULL REFERENCES managed_actors(uid),
    actor_incarnation INTEGER NOT NULL CHECK (actor_incarnation > 0),
    pool_uid TEXT NOT NULL REFERENCES managed_pools(uid),
    pool_revision INTEGER NOT NULL CHECK (pool_revision > 0),
    request_digest TEXT NOT NULL,
    challenge_digest TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('reserved','unconfirmed')),
    created_at TEXT NOT NULL,
    FOREIGN KEY (swarm_id, sequence) REFERENCES swarm_steps(swarm_id, sequence) ON DELETE CASCADE,
    PRIMARY KEY (swarm_id, sequence),
    UNIQUE (swarm_id, request_id),
    UNIQUE (actor_uid)
)`

// Триггеры целостности намерений оплачены ревью (#163); ключ перевешан
// с запуска на рой без изменения смысла (#206). Регистрация — в общем
// списке схемы: pipelinePreparationFullSchema и
// verifyPipelinePreparationsSchema в registry.go.

// pipelinePreparationTriggers — триггеры целостности намерений: актёр
// неизменен до подтверждения, связка и посещения защищены от подмены и
// удаления (#163). Семантика оплачена ревью (#205).
func pipelinePreparationTriggers() []struct{ name, statement string } {
	return []struct{ name, statement string }{
		{"pipeline_preparations_actor_fence", pipelinePreparationActorFence},
		{"pipeline_preparations_actor_no_replace", pipelinePreparationActorNoReplace},
		{"pipeline_preparations_no_delete", pipelinePreparationNoDelete},
		{"pipeline_preparations_no_replace", pipelinePreparationNoReplace},
		{"pipeline_preparations_update_fence", pipelinePreparationUpdateFence},
		{"pipeline_preparations_step_no_replace", pipelinePreparationStepNoReplace},
		{"pipeline_preparations_step_update_fence", pipelinePreparationStepUpdateFence},
		{"pipeline_preparations_swarm_no_delete", pipelinePreparationSwarmNoDelete},
	}
}

// ReservePipelinePreparation is closed (#208): it reserved a new dedicated
// managed actor for a workflow visit, and a workflow node no longer gets an
// actor of its own. A node names a role, the swarm binds the role to an
// existing actor (BindSwarmRole) and a step addresses that actor
// (AppendPipelineVisit). It returns ErrPipelineNodeActorCreation before any
// read or write, whatever the request or authorization.
func (s *Store) ReservePipelinePreparation(_ context.Context, _ PipelinePreparationRequest, _ func(context.Context) bool) (PipelinePreparation, error) {
	return PipelinePreparation{}, ErrPipelineNodeActorCreation
}

// RecoverPipelinePreparations marks interrupted attempts unconfirmed without retrying or freeing actors.
// Call only with exclusive ownership after the previous server has stopped.
func (s *Store) RecoverPipelinePreparations(ctx context.Context) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarm_steps SET state='unconfirmed' WHERE state='preparing' AND EXISTS (SELECT 1 FROM pipeline_preparations p WHERE p.swarm_id=swarm_steps.swarm_id AND p.sequence=swarm_steps.sequence AND p.state='reserved')`); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_preparations SET state='unconfirmed' WHERE state='reserved'`)
		return err
	})
}
