package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// EphemeralChildCreateGrant is internal allocation-bound history, not a public
// DTO or a provider receipt. Reserved means Create may have happened. Only a
// successful ReserveEphemeralChildCreate with Replay=false permits one subsequent
// Create; a getter, check, retry or restart never grants fresh permission.
// No ownership nonce, CTID or other private physical fields are returned.
type EphemeralChildCreateGrant struct {
	IntentID, ActorUID                              string
	ActorIncarnation                                int64
	CreateOperationID, State, CreatedAt, ReservedAt string
	Replay                                          bool
}

// GetEphemeralChildCreateGrant reads retained server-only history. It establishes
// neither visibility nor current authority and never authorizes physical work.
func (s *Store) GetEphemeralChildCreateGrant(ctx context.Context, intentID string) (EphemeralChildCreateGrant, error) {
	var out EphemeralChildCreateGrant
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT g.intent_id,a.actor_uid,a.actor_incarnation,a.create_operation_id,g.state,g.created_at,g.reserved_at FROM ephemeral_child_create_grants g JOIN ephemeral_child_allocations a ON a.intent_id=g.intent_id WHERE g.intent_id=?`, intentID).Scan(&out.IntentID, &out.ActorUID, &out.ActorIncarnation, &out.CreateOperationID, &out.State, &out.CreatedAt, &out.ReservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EphemeralChildCreateGrant{}, ErrNotFound
	}
	if err != nil {
		return EphemeralChildCreateGrant{}, err
	}
	out.Replay = true
	return out, nil
}

func (s *Store) currentEphemeralCreateScope(ctx context.Context, intentID string, authorize SwarmChildAuthorize) error {
	intent, allocation, err := s.currentEphemeralChildBindingScope(ctx, intentID, "create_ephemeral", false, false, authorize)
	if err != nil {
		return err
	}
	actor, err := s.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		return err
	}
	if actor.OperationID != allocation.CreateOperationID || actor.Operation != "create" || actor.Stage != "create" || actor.Phase != "provisioning" || actor.Desired != "running" || actor.UPID != "" || actor.Error != "" || allocation.ActorIncarnation != 1 {
		return ErrSwarmChildConflict
	}
	if err := s.checkEphemeralPhysicalBinding(ctx, intent, allocation, actor, true); err != nil {
		return err
	}
	if _, err := s.CurrentActorOperation(ctx, actor.UID); err == nil {
		return ErrSwarmChildConflict
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	var intervened bool
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_interventions WHERE actor_uid=? AND incarnation=?)`, actor.UID, actor.Incarnation).Scan(&intervened); err != nil {
		return err
	}
	if intervened {
		return ErrSwarmChildConflict
	}
	return nil
}

// Physical-привязка allocation неизменна при start/delete: хеш использует
// исходный create operation, никогда текущую lifecycle operation.
func (s *Store) checkEphemeralPhysicalBinding(ctx context.Context, intent EphemeralChildIntent, allocation EphemeralChildAllocation, actor ManagedActor, enabled bool) error {
	pool, err := s.GetManagedPool(ctx, intent.PoolUID)
	if err != nil {
		return err
	}
	if (enabled && pool.State != "enabled") || (pool.State != "enabled" && pool.State != "draining") || pool.Template != intent.Template || pool.SourceFingerprint != intent.SourceFingerprint || sourceFingerprint(pool.Source) != intent.SourceFingerprint || actor.ClusterIdentity != pool.Source.ClusterIdentity {
		return ErrManagedBinding
	}
	_, hash, err := s.ephemeralChildAllocation(ctx, intent.ID)
	if err != nil {
		return err
	}
	reservation := EphemeralActorReservation{UID: actor.UID, OwnerNonce: actor.OwnerNonce, OperationID: allocation.CreateOperationID, CTID: actor.CTID, Resources: actor.Resources}
	encoded, err := json.Marshal(reservation)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	if hex.EncodeToString(sum[:]) != hash {
		return ErrManagedBinding
	}
	return nil
}

// ReserveEphemeralChildCreate commits the sole queued-to-reserved transition with
// FULL sync in a non-nested transaction before a future provider call. Explicit
// create_ephemeral authority uses the transaction context for local reads only,
// with no network or effects. The executor may still be provisioning. Exact
// retries reauthorize but return Replay=true history, never permission to repeat.
// Legacy/unknown history cannot be issued, reset, timed out or rearmed.
func (s *Store) ReserveEphemeralChildCreate(ctx context.Context, intentID string, authorize SwarmChildAuthorize) (EphemeralChildCreateGrant, error) {
	if !validPipelineField(intentID) {
		return EphemeralChildCreateGrant{}, ErrSwarmChildConflict
	}
	var result EphemeralChildCreateGrant
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		if err := s.currentEphemeralCreateScope(ctx, intentID, authorize); err != nil {
			return err
		}
		var err error
		result, err = s.GetEphemeralChildCreateGrant(ctx, intentID)
		if err != nil {
			return err
		}
		switch result.State {
		case "reserved":
			return nil
		case "queued":
			result.State, result.ReservedAt, result.Replay = "reserved", nowRFC3339(), false
			changed, err := s.conn(ctx).ExecContext(ctx, `UPDATE ephemeral_child_create_grants SET state='reserved',reserved_at=? WHERE intent_id=? AND state='queued'`, result.ReservedAt, intentID)
			if err != nil {
				return err
			}
			count, err := changed.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return ErrSwarmChildConflict
			}
			return nil
		default:
			return ErrSwarmChildConflict
		}
	})
	if err != nil {
		return EphemeralChildCreateGrant{}, err
	}
	return result, nil
}

// CheckEphemeralChildCreate rechecks the exact reserved scope immediately before
// the future provider call. It is read-only and does not grant permission: the
// adapter must already hold its successful fresh durable reservation result.
func (s *Store) CheckEphemeralChildCreate(ctx context.Context, intentID string, authorize SwarmChildAuthorize) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.currentEphemeralCreateScope(ctx, intentID, authorize); err != nil {
			return err
		}
		grant, err := s.GetEphemeralChildCreateGrant(ctx, intentID)
		if err != nil {
			return err
		}
		if grant.State != "reserved" {
			return ErrSwarmChildConflict
		}
		return nil
	})
}
