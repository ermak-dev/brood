package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"brood.dev/go/brood/pipeline"
)

var (
	// ErrPipelinePreparationConflict indicates that a preparation identity is already bound.
	ErrPipelinePreparationConflict = errors.New("pipeline preparation conflicts with an existing attempt")
	// ErrPipelinePreparationDenied indicates that authorization no longer permits preparation.
	ErrPipelinePreparationDenied = errors.New("pipeline preparation is not authorized")
)

// PipelinePreparationRequest identifies a new dedicated actor and its workflow visit.
// The caller must bind authorize to the authenticated user's current session.
type PipelinePreparationRequest struct {
	SwarmID, RunID, NodeID, RequestID, UserID string
	Sequence                                  int64
	Actor                                     ManagedActor
}

// PipelinePreparation is an allocation intent, not permission to deliver work.
// Challenge is returned only after the first successful commit; a replay never exposes it.
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
const pipelinePreparationNoDelete = `CREATE TRIGGER pipeline_preparations_no_delete BEFORE DELETE ON pipeline_preparations WHEN EXISTS (SELECT 1 FROM pipeline_runs WHERE id=OLD.run_id) BEGIN SELECT RAISE(ABORT, 'pipeline preparation is immutable'); END`
const pipelinePreparationNoReplace = `CREATE TRIGGER pipeline_preparations_no_replace BEFORE INSERT ON pipeline_preparations WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE (run_id=NEW.run_id AND (sequence=NEW.sequence OR request_id=NEW.request_id)) OR actor_uid=NEW.actor_uid OR challenge_digest=NEW.challenge_digest) BEGIN SELECT RAISE(ABORT, 'pipeline preparation is immutable'); END`
const pipelinePreparationUpdateFence = `CREATE TRIGGER pipeline_preparations_update_fence BEFORE UPDATE ON pipeline_preparations WHEN NEW.run_id IS NOT OLD.run_id OR NEW.sequence IS NOT OLD.sequence OR NEW.node_id IS NOT OLD.node_id OR NEW.request_id IS NOT OLD.request_id OR NEW.actor_uid IS NOT OLD.actor_uid OR NEW.actor_incarnation IS NOT OLD.actor_incarnation OR NEW.pool_uid IS NOT OLD.pool_uid OR NEW.pool_revision IS NOT OLD.pool_revision OR NEW.request_digest IS NOT OLD.request_digest OR NEW.challenge_digest IS NOT OLD.challenge_digest OR NEW.created_at IS NOT OLD.created_at OR (OLD.state='unconfirmed' AND NEW.state!='unconfirmed') BEGIN SELECT RAISE(ABORT, 'pipeline preparation is immutable'); END`
const pipelinePreparationStepNoReplace = `CREATE TRIGGER pipeline_preparations_step_no_replace BEFORE INSERT ON pipeline_steps WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE run_id=NEW.run_id) BEGIN SELECT RAISE(ABORT, 'pipeline preparation visit is unresolved'); END`
const pipelinePreparationStepUpdateFence = `CREATE TRIGGER pipeline_preparations_step_update_fence BEFORE UPDATE ON pipeline_steps WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE run_id=OLD.run_id AND sequence=OLD.sequence) AND (NEW.run_id IS NOT OLD.run_id OR NEW.sequence IS NOT OLD.sequence OR NEW.node_id IS NOT OLD.node_id OR NEW.actor_uid IS NOT OLD.actor_uid OR NEW.actor_incarnation IS NOT OLD.actor_incarnation OR NEW.request_id IS NOT OLD.request_id OR NEW.answer IS NOT OLD.answer OR NEW.outcome IS NOT OLD.outcome OR NEW.native_actor_name IS NOT OLD.native_actor_name OR NEW.native_template_name IS NOT OLD.native_template_name OR NEW.native_template_uid IS NOT OLD.native_template_uid OR NEW.native_template_digest IS NOT OLD.native_template_digest OR NOT ((OLD.state='preparing' AND NEW.state IN ('preparing','unconfirmed')) OR (OLD.state='unconfirmed' AND NEW.state='unconfirmed'))) BEGIN SELECT RAISE(ABORT, 'pipeline preparation visit is immutable'); END`
const pipelinePreparationRunNoDelete = `CREATE TRIGGER pipeline_preparations_run_no_delete BEFORE DELETE ON pipeline_runs WHEN EXISTS (SELECT 1 FROM pipeline_preparations WHERE run_id=OLD.id) BEGIN SELECT RAISE(ABORT, 'pipeline preparation remains unresolved'); END`

const pipelinePreparationSchema = `CREATE TABLE pipeline_preparations (
    run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
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
    FOREIGN KEY (run_id, sequence) REFERENCES pipeline_steps(run_id, sequence) ON DELETE CASCADE,
    PRIMARY KEY (run_id, sequence),
    UNIQUE (run_id, request_id),
    UNIQUE (actor_uid)
)`

// Триггеры целостности намерений оплачены ревью (#163); регистрация
// перенесена в общий список схемы: pipelinePreparationFullSchema и
// verifyPipelinePreparationsSchema в registry.go (#205).

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
		{"pipeline_preparations_run_no_delete", pipelinePreparationRunNoDelete},
	}
}

// ReservePipelinePreparation atomically binds a new managed actor allocation to a visit.
// It refuses an outer transaction so the returned fresh challenge implies a committed intent.
// Neither this reservation nor the challenge authorizes actor startup or task delivery.
func (s *Store) ReservePipelinePreparation(ctx context.Context, req PipelinePreparationRequest, authorize func(context.Context) bool) (PipelinePreparation, error) {
	if _, nested := ctx.Value(txKey{}).(*sql.Tx); nested {
		return PipelinePreparation{}, errors.New("pipeline preparation cannot be nested in a transaction")
	}
	if authorize == nil || !authorize(ctx) {
		return PipelinePreparation{}, ErrPipelinePreparationDenied
	}
	if !validPipelineField(req.SwarmID) || !validPipelineField(req.RunID) || !validPipelineField(req.NodeID) || !validPipelineField(req.RequestID) || !validPipelineField(req.UserID) || req.Sequence < 1 {
		return PipelinePreparation{}, errors.New("invalid pipeline preparation identity")
	}
	input := req.Actor
	if input.SwarmID != req.SwarmID || (input.Role != "" && input.Role != "worker") || input.DelegatorID != "" || input.ParentUID != "" {
		return PipelinePreparation{}, errors.New("pipeline preparation requires a dedicated worker in the run's swarm")
	}
	actor := ManagedActor{UID: input.UID, Name: input.Name, SwarmID: input.SwarmID, AtespaceUID: input.AtespaceUID, PoolUID: input.PoolUID, Resources: input.Resources, CTID: input.CTID, OwnerNonce: input.OwnerNonce, OperationID: input.OperationID, Role: "worker"}
	var result PipelinePreparation
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if !authorize(ctx) {
			return ErrPipelinePreparationDenied
		}
		user, err := s.GetUser(ctx, req.UserID)
		if err != nil || user.Disabled {
			return ErrPipelinePreparationDenied
		}
		swarm, err := s.GetSwarm(ctx, req.SwarmID)
		if err != nil || swarm.State != "active" {
			return ErrPipelinePreparationDenied
		}
		if !user.IsAdmin {
			member, err := s.IsMember(ctx, swarm.ProjectID, req.UserID)
			if err != nil {
				return err
			}
			if !member {
				return ErrPipelinePreparationDenied
			}
			role, err := s.GetSwarmRole(ctx, req.SwarmID, req.UserID)
			if errors.Is(err, ErrNotFound) {
				return ErrPipelinePreparationDenied
			}
			if err != nil {
				return err
			}
			if role != "owner" && role != "editor" {
				return ErrPipelinePreparationDenied
			}
		}
		run, err := s.GetPipelineRun(ctx, req.SwarmID, req.RunID)
		if err != nil {
			return err
		}
		if run.Source == nil || (run.Status != "running" && run.Status != "waiting_human") {
			return ErrPipelinePreparationConflict
		}
		if input.Origin != "" && input.Origin != run.Source.Origin {
			return ErrPipelinePreparationConflict
		}
		actor.Origin = run.Source.Origin
		doc, err := pipeline.Parse(string(run.Graph), "pipeline")
		if err != nil {
			return err
		}
		if err = doc.Validate(); err != nil {
			return err
		}
		node, ok := doc.Nodes[req.NodeID]
		if !ok || node.Type != "actor" || node.ManagedPool == "" {
			return ErrPipelinePreparationConflict
		}
		pool, err := s.GetManagedPool(ctx, actor.PoolUID)
		if err != nil {
			return err
		}
		if pool.Name != node.ManagedPool || pool.State != "enabled" {
			return ErrPipelinePreparationConflict
		}
		fingerprintInput := struct {
			SwarmID, RunID, NodeID, RequestID, UserID string
			Sequence                                  int64
			Actor                                     ManagedActor
		}{req.SwarmID, req.RunID, req.NodeID, req.RequestID, req.UserID, req.Sequence, actor}
		encoded, err := json.Marshal(fingerprintInput)
		if err != nil {
			return err
		}
		digestBytes := sha256.Sum256(encoded)
		digest := hex.EncodeToString(digestBytes[:])
		var storedDigest, preparationState string
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT request_digest,actor_uid,actor_incarnation,pool_uid,pool_revision,state FROM pipeline_preparations WHERE run_id=? AND (sequence=? OR request_id=?)`, req.RunID, req.Sequence, req.RequestID).Scan(&storedDigest, &result.ActorUID, &result.Incarnation, &result.PoolUID, &result.PoolRevision, &preparationState)
		if err == nil {
			if storedDigest != digest || preparationState != "reserved" || pool.UID != result.PoolUID || pool.Revision != result.PoolRevision {
				return ErrPipelinePreparationConflict
			}
			current, err := s.GetManagedActor(ctx, result.ActorUID)
			if err != nil || current.Incarnation != result.Incarnation || current.SwarmID != req.SwarmID || current.Desired != "running" || (current.Phase != "provisioning" && current.Phase != "running") {
				return ErrPipelinePreparationConflict
			}
			var visitCurrent bool
			if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pipeline_steps WHERE run_id=? AND sequence=? AND sequence=(SELECT MAX(sequence) FROM pipeline_steps WHERE run_id=?) AND node_id=? AND state='preparing' AND actor_uid=? AND actor_incarnation=? AND request_id=?)`, req.RunID, req.Sequence, req.RunID, req.NodeID, result.ActorUID, result.Incarnation, req.RequestID).Scan(&visitCurrent); err != nil {
				return err
			}
			if !visitCurrent {
				return ErrPipelinePreparationConflict
			}
			result.Replay = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count, latest int64
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT count(*),COALESCE(MAX(sequence),0) FROM pipeline_steps WHERE run_id=?`, req.RunID).Scan(&count, &latest); err != nil {
			return err
		}
		if count == 0 {
			if req.Sequence != 1 || req.NodeID != doc.Start {
				return ErrPipelinePreparationConflict
			}
		} else {
			var nodeID, state string
			if latest != req.Sequence {
				return ErrPipelinePreparationConflict
			}
			if err := s.conn(ctx).QueryRowContext(ctx, `SELECT node_id,state FROM pipeline_steps WHERE run_id=? AND sequence=?`, req.RunID, latest).Scan(&nodeID, &state); err != nil {
				return err
			}
			if nodeID != req.NodeID || state != "pending" {
				return ErrPipelinePreparationConflict
			}
		}
		reserved, err := s.ReserveManagedActor(ctx, actor)
		if err != nil {
			return err
		}
		if count == 0 {
			_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_steps(run_id,sequence,node_id,state,actor_uid,actor_incarnation,request_id) VALUES (?,?,?,'preparing',?,?,?)`, req.RunID, req.Sequence, req.NodeID, reserved.UID, reserved.Incarnation, req.RequestID)
		} else {
			var updated sql.Result
			updated, err = s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_steps SET state='preparing',actor_uid=?,actor_incarnation=?,request_id=? WHERE run_id=? AND sequence=? AND node_id=? AND state='pending' AND actor_uid IS NULL AND actor_incarnation IS NULL AND request_id IS NULL`, reserved.UID, reserved.Incarnation, req.RequestID, req.RunID, req.Sequence, req.NodeID)
			if err == nil {
				var n int64
				n, err = updated.RowsAffected()
				if err == nil && n != 1 {
					return ErrPipelinePreparationConflict
				}
			}
		}
		if err != nil {
			return err
		}
		var challenge [32]byte
		if _, err := rand.Read(challenge[:]); err != nil {
			return errors.New("cannot generate pipeline preparation challenge")
		}
		hash := sha256.Sum256(challenge[:])
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_preparations(run_id,sequence,node_id,request_id,actor_uid,actor_incarnation,pool_uid,pool_revision,request_digest,challenge_digest,state,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,'reserved',?)`, req.RunID, req.Sequence, req.NodeID, req.RequestID, reserved.UID, reserved.Incarnation, pool.UID, pool.Revision, digest, hex.EncodeToString(hash[:]), nowRFC3339())
		if err != nil {
			return err
		}
		result = PipelinePreparation{ActorUID: reserved.UID, Incarnation: reserved.Incarnation, PoolUID: pool.UID, PoolRevision: pool.Revision, Challenge: hex.EncodeToString(challenge[:])}
		return nil
	})
	if err != nil {
		return PipelinePreparation{}, err
	}
	return result, nil
}

// RecoverPipelinePreparations marks interrupted attempts unconfirmed without retrying or freeing actors.
// Call only with exclusive ownership after the previous server has stopped.
func (s *Store) RecoverPipelinePreparations(ctx context.Context) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_steps SET state='unconfirmed' WHERE state='preparing' AND EXISTS (SELECT 1 FROM pipeline_preparations p WHERE p.run_id=pipeline_steps.run_id AND p.sequence=pipeline_steps.sequence AND p.state='reserved')`); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_preparations SET state='unconfirmed' WHERE state='reserved'`)
		return err
	})
}
