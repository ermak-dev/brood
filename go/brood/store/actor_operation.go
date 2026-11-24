package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
)

// actorOperationSchema — бронь операции над агентом: не больше одной строки
// на агента. Снятая бронь удаляется, истории нет.
//
//go:embed actor_operation.sql
var actorOperationSchema string

// ErrOperationConflict identifies an occupied agent reservation or a stale
// operation identity.
//
// Текст «coordination conflict» остался от прежнего слоя: панель показывает
// его в отказе start/stop/rm/reset, и он не меняется вместе с именем типа.
var ErrOperationConflict = errors.New("coordination conflict")

// OperationConflictError is a stable domain conflict, not a SQLite constraint error.
type OperationConflictError struct{ Reason string }

func (e *OperationConflictError) Error() string { return "coordination conflict: " + e.Reason }
func (e *OperationConflictError) Unwrap() error { return ErrOperationConflict }
func newOperationConflict(reason string) error  { return &OperationConflictError{Reason: reason} }

// ActorOperation reserves one incarnation during a workspace side effect.
// State is inflight, or unconfirmed after a server restart interrupted it.
// An unconfirmed reservation cannot expire into permission to repeat the effect.
type ActorOperation struct {
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
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE actor_operations SET state='unconfirmed'`)
	return err
}

// CurrentActorOperation returns the reservation held on the agent.
func (s *Store) CurrentActorOperation(ctx context.Context, uid string) (ActorOperation, error) {
	var op ActorOperation
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT actor_uid,generation,operation_id,caller,operation,state,owner FROM actor_operations WHERE actor_uid=?`, uid).
		Scan(&op.UID, &op.Generation, &op.OperationID, &op.Caller, &op.Operation, &op.State, &op.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return ActorOperation{}, fmt.Errorf("%w: actor operation %q", ErrNotFound, uid)
	}
	return op, err
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
		return newOperationConflict("a restart or slash command on this agent is still running or its outcome is unconfirmed")
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
		return ActorOperation{}, newOperationConflict("only a slash command takes a session reservation")
	}
	return s.acquireAuthorizedActorOperation(ctx, target, id, caller, operation, false, authorize)
}

// AcquireOwnerActorOperation reserves a workspace or session operation of the
// swarm owner. Authorize reads local authority with ctx; nil denies.
func (s *Store) AcquireOwnerActorOperation(ctx context.Context, target ManagedActor, id, caller, operation string, authorize func(context.Context) error) (ActorOperation, error) {
	return s.acquireAuthorizedActorOperation(ctx, target, id, caller, operation, true, authorize)
}

func (s *Store) acquireAuthorizedActorOperation(ctx context.Context, target ManagedActor, id, caller, operation string, owner bool, authorize func(context.Context) error) (ActorOperation, error) {
	if authorize == nil {
		return ActorOperation{}, newOperationConflict("authority for the operation is required")
	}
	return s.acquireActorOperation(ctx, target.UID, target.Incarnation, id, caller, operation, owner, func(ctx context.Context, current ManagedActor) error {
		if current.SwarmID != target.SwarmID {
			return newOperationConflict("the agent moved to another swarm")
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
		return op, newOperationConflict("invalid workspace operation identity")
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
			return newOperationConflict("the agent is not running")
		}
		if err := guard(ctx, a); err != nil {
			return err
		}
		op = ActorOperation{UID: uid, Generation: generation, OperationID: id, Caller: caller, Operation: operation, State: "inflight", Owner: owner}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_operations(actor_uid,generation,operation_id,caller,operation,state,owner) VALUES (?,?,?,?,?,?,?)`, op.UID, op.Generation, op.OperationID, op.Caller, op.Operation, op.State, op.Owner)
		return err
	})
	return op, err
}

// FinishActorOperation releases the reservation whatever the outcome; confirmed
// is not stored. The exact operation identity prevents an old handler from
// releasing a successor.
func (s *Store) FinishActorOperation(ctx context.Context, op ActorOperation, confirmed bool) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		current, err := s.CurrentActorOperation(ctx, op.UID)
		if err != nil {
			return err
		}
		if current.OperationID != op.OperationID || current.Generation != op.Generation || current.Caller != op.Caller || current.Operation != op.Operation {
			return newOperationConflict("workspace operation ownership changed")
		}
		// Бронь снимается по завершении при любом исходе. Неизвестный исход
		// вызывающий получает в ответе команды и проверяет сам (tail, agents);
		// вечная бронь закрыла бы агента для всех, и снять её было бы нечем.
		_, err = s.conn(ctx).ExecContext(ctx, `DELETE FROM actor_operations WHERE actor_uid=? AND operation_id=?`, op.UID, op.OperationID)
		return err
	})
}
