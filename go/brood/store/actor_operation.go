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
	ProjectID     string
	SwarmID       string
	UID           string
	Generation    int64
	OperationID   string
	Caller        string
	Operation     string
	State         string
	Owner         bool
	InterruptedBy string `json:",omitempty"`
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

// AcquireActorOperation atomically excludes dispatch, landing, and other workspace operations.
// workspace_attest is the swarm's workspace attestation (#212): it stops and
// restarts the whole actor like restart, so it takes the same reservation.
func (s *Store) AcquireActorOperation(ctx context.Context, uid string, generation int64, id, caller, operation string) (ActorOperation, error) {
	return s.acquireActorOperation(ctx, uid, generation, id, caller, operation, false, s.guardActorCoordination)
}

func sessionMutationOperation(operation string) bool {
	return operation == "slash" || operation == "session_terminal" || operation == "terminal_stream"
}

// AcquireActorSessionOperation reserves a session mutation while retaining the
// existing dispatch owner's control. Authorize must only read local authority
// with ctx, without network I/O. Nil denies. The snapshot pins the target scope.
func (s *Store) AcquireActorSessionOperation(ctx context.Context, target ManagedActor, id, caller, operation string, source CoordinationActor, human string, authorize func(context.Context) error) (ActorOperation, error) {
	if !sessionMutationOperation(operation) || authorize == nil || human == "" {
		return ActorOperation{}, newCoordinationConflict("session mutation authority is required")
	}
	return s.acquireActorOperation(ctx, target.UID, target.Incarnation, id, caller, operation, false, func(ctx context.Context, current ManagedActor) error {
		if current.SwarmID != target.SwarmID {
			return newCoordinationConflict("session target scope changed")
		}
		if err := authorize(ctx); err != nil {
			return err
		}
		if _, err := s.CurrentActorOperation(ctx, current.UID); err == nil {
			return newCoordinationConflict("session operation is active or unconfirmed")
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		claims, err := s.ActorCoordinationClaims(ctx, current.UID)
		if err != nil {
			return err
		}
		for _, claim := range claims {
			if source.UID != "" && claim.Source == source || source.UID == "" && claim.HumanID == human {
				continue
			}
			return newCoordinationConflict("session control belongs to another dispatch owner")
		}
		return nil
	})
}

func (s *Store) acquireActorOperation(ctx context.Context, uid string, generation int64, id, caller, operation string, owner bool, guard func(context.Context, ManagedActor) error) (ActorOperation, error) {
	var op ActorOperation
	if id == "" || caller == "" || len(caller) > 512 || (operation != "restart" && operation != "wipe" && operation != "recycle" && operation != "workspace_attest" && !sessionMutationOperation(operation)) {
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
			return newCoordinationConflict("actor incarnation is not ready for workspace control")
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
		if confirmed && current.InterruptedBy != "" {
			return newCoordinationConflict("owner interrupted this workspace operation; late acknowledgement cannot release its uncertain effects")
		}
		// Операция владельца роя бронь не удерживает и при неизвестном
		// исходе: владелец видит агента в терминале и решает сам, а
		// вечная бронь закрыла бы ему и терминал, и повтор.
		if confirmed || current.Owner {
			_, err = s.conn(ctx).ExecContext(ctx, `UPDATE coordination_claims SET slot=NULL WHERE project_id=? AND kind='workspace' AND request_id=? AND slot=?`, op.ProjectID, op.OperationID, op.UID)
			return err
		}
		current.State = "unconfirmed"
		result, err := s.conn(ctx).ExecContext(ctx, `UPDATE coordination_claims SET payload=? WHERE project_id=? AND kind='workspace' AND request_id=? AND slot=?`, encodeManaged(current), op.ProjectID, op.OperationID, op.UID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("workspace reservation was not retained")
		}
		return nil
	})
}

// AcquireOwnerActorOperation reserves a workspace or session operation of the
// swarm owner. Authorize reads local authority with ctx; nil denies. The
// owner is not held by coordination claims of other actors, only by another
// active or unconfirmed operation on the same worker.
func (s *Store) AcquireOwnerActorOperation(ctx context.Context, target ManagedActor, id, caller, operation string, authorize func(context.Context) error) (ActorOperation, error) {
	if authorize == nil {
		return ActorOperation{}, newCoordinationConflict("owner authority is required")
	}
	return s.acquireActorOperation(ctx, target.UID, target.Incarnation, id, caller, operation, true, func(ctx context.Context, current ManagedActor) error {
		if current.SwarmID != target.SwarmID {
			return newCoordinationConflict("session target scope changed")
		}
		if err := authorize(ctx); err != nil {
			return err
		}
		if _, err := s.CurrentActorOperation(ctx, current.UID); err == nil {
			return newCoordinationConflict("workspace operation is active or unconfirmed")
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		return nil
	})
}
