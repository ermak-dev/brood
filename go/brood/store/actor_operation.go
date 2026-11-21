package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ActorOperation reserves one incarnation during a workspace side effect.
// An unconfirmed reservation cannot expire into permission to repeat the effect.
type ActorOperation struct {
	ProjectID   string
	SwarmID     string
	UID         string
	Generation  int64
	OperationID string
	Caller      string
	Operation   string
	State       string
	Owner       bool
}

// RecoverActorOperations marks abandoned inflight effects as unconfirmed without releasing them.
// Call only during exclusive server startup after the preceding server has stopped.
func (s *Store) RecoverActorOperations(ctx context.Context) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE coordination_claims SET payload=json_set(payload,'$.State','unconfirmed') WHERE kind='workspace' AND slot IS NOT NULL`)
		return err
	})
}

// CurrentActorOperation returns an active workspace operation, not historical results.
func (s *Store) CurrentActorOperation(ctx context.Context, uid string) (ActorOperation, error) {
	var op ActorOperation
	var payload string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT payload FROM coordination_claims WHERE kind='workspace' AND slot=?`, uid).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return op, fmt.Errorf("%w: actor operation %q", ErrNotFound, uid)
	}
	if err != nil {
		return op, err
	}
	if err := json.Unmarshal([]byte(payload), &op); err != nil {
		return op, err
	}
	return op, nil
}

// AcquireActorOperation reserves the agent for a workspace operation of a
// master. It is refused while another operation on the same agent is active
// or unconfirmed.
func (s *Store) AcquireActorOperation(ctx context.Context, uid string, generation int64, id, caller, operation string) (ActorOperation, error) {
	return s.acquireActorOperation(ctx, uid, generation, id, caller, operation, false, s.guardActorOperation)
}

// guardActorOperation refuses while a workspace operation on the agent is
// active or its outcome is unconfirmed.
func (s *Store) guardActorOperation(ctx context.Context, a ManagedActor) error {
	if _, err := s.CurrentActorOperation(ctx, a.UID); err == nil {
		return newCoordinationConflict("a restart or slash command on this agent is still running or its outcome is unconfirmed")
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// AcquireActorSessionOperation reserves a slash command. Authorize must
// only read local authority with ctx, without network I/O. Nil denies. The
// snapshot pins the target scope.
func (s *Store) AcquireActorSessionOperation(ctx context.Context, target ManagedActor, id, caller, operation string, authorize func(context.Context) error) (ActorOperation, error) {
	if operation != "slash" {
		return ActorOperation{}, newCoordinationConflict("only a slash command takes a session reservation")
	}
	return s.acquireAuthorizedActorOperation(ctx, target, id, caller, operation, false, authorize)
}

// AcquireOwnerActorOperation reserves a workspace or session operation of the
// swarm owner. Authorize reads local authority with ctx; nil denies. An
// owner's reservation is released even when its outcome is unknown
// (FinishActorOperation).
func (s *Store) AcquireOwnerActorOperation(ctx context.Context, target ManagedActor, id, caller, operation string, authorize func(context.Context) error) (ActorOperation, error) {
	return s.acquireAuthorizedActorOperation(ctx, target, id, caller, operation, true, authorize)
}

func (s *Store) acquireAuthorizedActorOperation(ctx context.Context, target ManagedActor, id, caller, operation string, owner bool, authorize func(context.Context) error) (ActorOperation, error) {
	if authorize == nil {
		return ActorOperation{}, newCoordinationConflict("authority for the operation is required")
	}
	return s.acquireActorOperation(ctx, target.UID, target.Incarnation, id, caller, operation, owner, func(ctx context.Context, current ManagedActor) error {
		if current.SwarmID != target.SwarmID {
			return newCoordinationConflict("the agent moved to another swarm")
		}
		if err := authorize(ctx); err != nil {
			return err
		}
		return s.guardActorOperation(ctx, current)
	})
}

func (s *Store) acquireActorOperation(ctx context.Context, uid string, generation int64, id, caller, operation string, owner bool, guard func(context.Context, ManagedActor) error) (ActorOperation, error) {
	var op ActorOperation
	if id == "" || caller == "" || len(caller) > 512 || (operation != "restart" && operation != "slash") {
		return op, newCoordinationConflict("invalid workspace operation identity")
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		a, err := s.GetManagedActor(ctx, uid)
		if err != nil {
			return err
		}
		sw, err := s.GetSwarm(ctx, a.SwarmID)
		if err != nil {
			return err
		}
		if a.Incarnation != generation || a.Desired != "running" || a.Phase != "running" || a.Stage != "done" || a.Operation != "start" || sw.State != "active" {
			return newCoordinationConflict("the agent is not running")
		}
		if err := guard(ctx, a); err != nil {
			return err
		}
		op = ActorOperation{ProjectID: sw.ProjectID, SwarmID: sw.ID, UID: uid, Generation: generation, OperationID: id, Caller: caller, Operation: operation, State: "inflight", Owner: owner}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES (?,'workspace',?,?,?)`, sw.ProjectID, id, encodeManaged(op), uid)
		return err
	})
	return op, err
}

// FinishActorOperation releases a confirmed operation or retains its uncertain outcome.
// The exact operation identity prevents an old handler from releasing a successor.
func (s *Store) FinishActorOperation(ctx context.Context, op ActorOperation, confirmed bool) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		current, err := s.CurrentActorOperation(ctx, op.UID)
		if err != nil {
			return err
		}
		if current.OperationID != op.OperationID || current.Generation != op.Generation || current.Caller != op.Caller || current.Operation != op.Operation {
			return newCoordinationConflict("workspace operation ownership changed")
		}
		// Бронь снимается по завершении при любом исходе. Неизвестный исход
		// вызывающий получает в ответе команды и проверяет сам (tail, agents);
		// вечная бронь закрыла бы агента для всех, и снять её было бы нечем.
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE coordination_claims SET slot=NULL WHERE project_id=? AND kind='workspace' AND request_id=? AND slot=?`, op.ProjectID, op.OperationID, op.UID)
		return err
	})
}
