package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
)

// ActorDeliveryInboxTarget identifies one exact logical message and the
// authenticated supervisor receiver selected for it. Digest is lowercase
// SHA-256 of the retained payload bytes, not a hash of a re-encoded message.
type ActorDeliveryInboxTarget struct {
	DeliveryID, SwarmID, SourceKind, SourceID string
	ActorUID                                  string
	Incarnation                               int64
	ReceiverID, Digest                        string
}

// ActorDeliveryInboxAdmitted is positive supervisor custody, not a receipt
// from the final consumer of the message.
const ActorDeliveryInboxAdmitted = "admitted_to_supervisor_inbox"

// ActorDeliveryInboxState exposes retained provenance without reissuing a permit
// or establishing the authority to deliver now.
type ActorDeliveryInboxState struct {
	Target           ActorDeliveryInboxTarget
	Custody          bool
	DispatchReserved bool
}

// ActorDeliveryInboxStatus reads a previously selected receiver and its retained
// evidence in one snapshot. An absent selection is not a retry permission.
func (s *Store) ActorDeliveryInboxStatus(ctx context.Context, id string) (ActorDeliveryInboxState, error) {
	var state ActorDeliveryInboxState
	v := &state.Target
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT d.id,d.swarm_id,d.source_kind,d.source_id,d.actor_uid,d.incarnation,i.receiver_id,i.digest,
 EXISTS(SELECT 1 FROM actor_delivery_inbox_custody WHERE delivery_id=i.delivery_id),
 EXISTS(SELECT 1 FROM actor_delivery_inbox_permits WHERE delivery_id=i.delivery_id)
 FROM actor_delivery_inbox_selections i JOIN actor_deliveries d ON d.id=i.delivery_id WHERE i.delivery_id=?`, id).
		Scan(&v.DeliveryID, &v.SwarmID, &v.SourceKind, &v.SourceID, &v.ActorUID, &v.Incarnation, &v.ReceiverID, &v.Digest, &state.Custody, &state.DispatchReserved)
	if errors.Is(err, sql.ErrNoRows) {
		return ActorDeliveryInboxState{}, ErrNotFound
	}
	if err != nil {
		return ActorDeliveryInboxState{}, err
	}
	return state, nil
}

// SelectActorDeliveryInbox durably binds a receiver before the first inbox
// operation. The callback checks current receiver authority in the transaction;
// the same receiver can be selected again but native attempts can never follow.
func (s *Store) SelectActorDeliveryInbox(ctx context.Context, target ActorDeliveryInboxTarget, authorize func(context.Context) bool) error {
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		return s.selectActorDeliveryInbox(ctx, target, authorize)
	})
}

// selectActorDeliveryInbox is only called inside a durable transaction.
func (s *Store) selectActorDeliveryInbox(ctx context.Context, target ActorDeliveryInboxTarget, authorize func(context.Context) bool) error {
	if err := s.checkActorDeliveryInboxTarget(ctx, target); err != nil {
		return err
	}
	if authorize == nil || !authorize(ctx) {
		return ErrActorDeliveryDenied
	}
	var receiver, digest string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT receiver_id,digest FROM actor_delivery_inbox_selections WHERE delivery_id=?`, target.DeliveryID).Scan(&receiver, &digest)
	if err == nil {
		if receiver != target.ReceiverID || digest != target.Digest {
			return ErrActorDeliveryConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var attempted bool
	if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_delivery_attempts WHERE delivery_id=?)`, target.DeliveryID).Scan(&attempted); err != nil {
		return err
	}
	if attempted {
		return ErrActorDeliveryConflict
	}
	_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_delivery_inbox_selections(delivery_id,receiver_id,digest,selected_at) VALUES(?,?,?,?)`,
		target.DeliveryID, target.ReceiverID, target.Digest, nowRFC3339())
	return err
}

// RecordActorDeliveryInboxCustody retains only positive custody for the exact
// selection. It never records a native outcome or changes the delivery source.
func (s *Store) RecordActorDeliveryInboxCustody(ctx context.Context, target ActorDeliveryInboxTarget, disposition string, authorize func(context.Context) bool) error {
	if disposition != ActorDeliveryInboxAdmitted {
		return ErrActorDeliveryConflict
	}
	return s.WithDurableTx(ctx, func(ctx context.Context) error {
		if err := s.checkActorDeliveryInboxSelection(ctx, target); err != nil {
			return err
		}
		if authorize == nil || !authorize(ctx) {
			return ErrActorDeliveryDenied
		}
		_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_delivery_inbox_custody(delivery_id,recorded_at)
 SELECT ?,? WHERE NOT EXISTS(SELECT 1 FROM actor_delivery_inbox_custody WHERE delivery_id=?)`, target.DeliveryID, nowRFC3339(), target.DeliveryID)
		return err
	})
}

// ReserveActorDeliveryInboxDispatch returns permission only after a FULL-sync
// commit. Authorize must match the exact target and launch to the current
// authenticated supervisor, using the transaction context for Store reads.
// A committed permit is never returned again, even if its response was lost.
func (s *Store) ReserveActorDeliveryInboxDispatch(ctx context.Context, target ActorDeliveryInboxTarget, launchID string, authorize func(context.Context, ActorDeliveryInboxTarget, string) bool) (string, error) {
	if len(launchID) < 1 || len(launchID) > 128 || strings.Trim(launchID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-:.") != "" {
		return "", ErrActorDeliveryConflict
	}
	var permitID string
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		if err := s.checkActorDeliveryInboxSelection(ctx, target); err != nil {
			return err
		}
		var eligible bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_delivery_inbox_custody WHERE delivery_id=?)
 AND NOT EXISTS(SELECT 1 FROM actor_delivery_inbox_permits WHERE delivery_id=?)`, target.DeliveryID, target.DeliveryID).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return ErrActorDeliveryConflict
		}
		if authorize == nil || !authorize(ctx, target, launchID) {
			return ErrActorDeliveryDenied
		}
		permitID = NewID()
		_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO actor_delivery_inbox_permits(delivery_id,permit_id,launch_id,reserved_at) VALUES(?,?,?,?)`,
			target.DeliveryID, permitID, launchID, nowRFC3339())
		return err
	})
	if err != nil {
		return "", err
	}
	return permitID, nil
}

func (s *Store) checkActorDeliveryInboxTarget(ctx context.Context, target ActorDeliveryInboxTarget) error {
	if target.DeliveryID == "" || len(target.ReceiverID) != 64 || strings.Trim(target.ReceiverID, "0123456789abcdef") != "" {
		return ErrActorDeliveryConflict
	}
	intent, err := s.GetActorDelivery(ctx, target.DeliveryID)
	if errors.Is(err, ErrNotFound) {
		return ErrActorDeliveryConflict
	}
	if err != nil {
		return err
	}
	in := intent.Input
	digest := sha256.Sum256([]byte(in.Payload))
	if intent.ProbeOnly || target.SwarmID != in.SwarmID || target.SourceKind != in.SourceKind || target.SourceID != in.SourceID ||
		target.ActorUID != in.ActorUID || target.Incarnation != in.Incarnation || target.ReceiverID == in.JournalID || target.Digest != hex.EncodeToString(digest[:]) {
		return ErrActorDeliveryConflict
	}
	uncertain, err := s.actorDeliveryCurrent(ctx, in)
	if uncertain || errors.Is(err, ErrNotFound) {
		return ErrActorDeliveryConflict
	}
	return err
}

func (s *Store) checkActorDeliveryInboxSelection(ctx context.Context, target ActorDeliveryInboxTarget) error {
	if err := s.checkActorDeliveryInboxTarget(ctx, target); err != nil {
		return err
	}
	var selected bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_delivery_inbox_selections
 WHERE delivery_id=? AND receiver_id=? AND digest=?)`, target.DeliveryID, target.ReceiverID, target.Digest).Scan(&selected)
	if err == nil && !selected {
		return ErrActorDeliveryConflict
	}
	return err
}

func (s *Store) actorDeliveryInboxSelected(ctx context.Context, deliveryID string) (bool, error) {
	var selected bool
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM actor_delivery_inbox_selections WHERE delivery_id=?)`, deliveryID).Scan(&selected)
	return selected, err
}
