package store

import (
	"context"
	"database/sql"
	"errors"
)

// OwnerManagedActorLineage identifies a retained derived operation and its exact
// predecessor. Origin is the original completed explicit owner reset, not a new
// intervention or permission. No current inventory or human ACL is implied.
type OwnerManagedActorLineage struct {
	UID              string
	Incarnation      int64
	OperationID      string
	Operation        string
	PriorIncarnation int64
	PriorOperationID string
	Origin           ActorOperation
}

// GetDerivedOwnerManagedActorOperation reads only an exact retained lineage link
// and its original completed reset evidence. Direct journal entries are not links.
// Missing or invalid identities return ErrNotFound. This does not authorize effects.
func (s *Store) GetDerivedOwnerManagedActorOperation(ctx context.Context, uid string, incarnation int64, operationID string) (OwnerManagedActorLineage, error) {
	var link OwnerManagedActorLineage
	if uid == "" || incarnation <= 0 || operationID == "" {
		return link, ErrNotFound
	}
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT l.actor_uid,l.incarnation,l.operation_id,l.operation,l.prior_incarnation,l.prior_operation_id,
 r.project_id,r.swarm_id,r.actor_uid,r.incarnation,r.id,r.caller,r.operation,r.state
 FROM owner_managed_actor_lineage l JOIN actor_interventions r ON r.id=l.origin_id
 WHERE l.actor_uid=? AND l.incarnation=? AND l.operation_id=? AND r.actor_uid=l.actor_uid AND r.operation='reset' AND r.state='completed'`, uid, incarnation, operationID).Scan(
		&link.UID, &link.Incarnation, &link.OperationID, &link.Operation, &link.PriorIncarnation, &link.PriorOperationID,
		&link.Origin.ProjectID, &link.Origin.SwarmID, &link.Origin.UID, &link.Origin.Generation, &link.Origin.OperationID, &link.Origin.Caller, &link.Origin.Operation, &link.Origin.State)
	if errors.Is(err, sql.ErrNoRows) {
		return OwnerManagedActorLineage{}, ErrNotFound
	}
	if err != nil {
		return OwnerManagedActorLineage{}, err
	}
	link.Origin.Owner = true
	return link, nil
}

// retainOwnerManagedActorLineage is called only after a real managed transition,
// inside its savepoint. Absent/unknown owner evidence never manufactures intent.
func (s *Store) retainOwnerManagedActorLineage(ctx context.Context, prior, next ManagedActor) error {
	if prior.Operation != "reset" {
		return nil
	}
	if next.Operation == "start" && (prior.Stage != "done" || prior.Phase != "stopped") {
		return nil
	}
	var root ActorOperation
	err := ErrNotFound
	if next.Operation != "start" {
		root, err = s.GetOwnerManagedActorOperation(ctx, prior.UID, prior.Incarnation, prior.OperationID)
	}
	if errors.Is(err, ErrNotFound) {
		link, lookupErr := s.GetDerivedOwnerManagedActorOperation(ctx, prior.UID, prior.Incarnation, prior.OperationID)
		if errors.Is(lookupErr, ErrNotFound) {
			return nil
		}
		if lookupErr != nil {
			return lookupErr
		}
		if link.Operation != "reset" {
			return nil
		}
		root, err = link.Origin, nil
	}
	if err != nil {
		return err
	}
	if root.State != "completed" || root.Operation != "reset" || root.UID != prior.UID || root.SwarmID != prior.SwarmID || next.SwarmID != prior.SwarmID || next.AtespaceUID != prior.AtespaceUID {
		return nil
	}
	sw, err := s.GetSwarm(ctx, prior.SwarmID)
	if err != nil {
		return err
	}
	if root.ProjectID != sw.ProjectID {
		return nil
	}
	result, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO owner_managed_actor_lineage(actor_uid,incarnation,operation_id,operation,prior_incarnation,prior_operation_id,origin_id) VALUES(?,?,?,?,?,?,?)`, next.UID, next.Incarnation, next.OperationID, next.Operation, prior.Incarnation, prior.OperationID, root.OperationID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRevisionConflict
	}
	return nil
}
