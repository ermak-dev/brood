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
}

// RecoverActorOperations marks abandoned inflight effects as unconfirmed without releasing them.
// Call only during exclusive server startup after the preceding server has stopped.
func (s *Store) RecoverActorOperations(ctx context.Context) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE coordination_claims SET payload=json_set(payload,'$.State','unconfirmed') WHERE kind='workspace' AND slot IS NOT NULL`)
	return err
}

// CurrentActorOperation returns an active workspace operation, not historical results.
func (s *Store) CurrentActorOperation(ctx context.Context, uid string) (ActorOperation, error) {
	var op ActorOperation
	var payload string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT payload FROM coordination_claims WHERE kind='workspace' AND slot=?`, uid).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return op, ErrNotFound
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
func (s *Store) AcquireActorOperation(ctx context.Context, uid string, generation int64, id, caller, operation string) (ActorOperation, error) {
	var op ActorOperation
	if id == "" || caller == "" || len(caller) > 512 || (operation != "restart" && operation != "wipe" && operation != "recycle") {
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
		if a.Incarnation != generation || a.Desired != "running" || a.Phase != "running" || a.Stage != "done" || a.Operation != "start" || sw.State != "active" || sw.AtespaceUID == nil || *sw.AtespaceUID != a.AtespaceUID {
			return newCoordinationConflict("actor incarnation is not ready for workspace control")
		}
		if err := s.guardActorCoordination(ctx, uid); err != nil {
			return err
		}
		op = ActorOperation{ProjectID: sw.ProjectID, SwarmID: sw.ID, UID: uid, Generation: generation, OperationID: id, Caller: caller, Operation: operation, State: "inflight"}
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
		if confirmed {
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
