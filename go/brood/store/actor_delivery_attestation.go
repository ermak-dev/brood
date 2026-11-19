package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// RecordActorStepAttestationDelivery atomically pins verified workspace evidence
// and the exact instruction payload before the source becomes deliverable.
// The caller supplies already verified guest attestation and current authority.
func (s *Store) RecordActorStepAttestationDelivery(ctx context.Context, a ActorStepAttempt, origin, commit string, in ActorDeliveryInput, authorize func(context.Context) bool) (ActorDeliveryIntent, error) {
	return s.recordActorStepAttestationTransport(ctx, a, origin, commit, in, "", authorize)
}

// RecordActorStepInboxAttestation commits a verified workspace, exact payload
// and its selected receiving inbox together. No recovery worker can see a
// deliverable native intent between attestation and selection of this inbox.
func (s *Store) RecordActorStepInboxAttestation(ctx context.Context, a ActorStepAttempt, origin, commit string, in ActorDeliveryInput, receiverID string, authorize func(context.Context) bool) (ActorDeliveryIntent, error) {
	if receiverID == "" {
		return ActorDeliveryIntent{}, ErrActorDeliveryConflict
	}
	return s.recordActorStepAttestationTransport(ctx, a, origin, commit, in, receiverID, authorize)
}

func (s *Store) recordActorStepAttestationTransport(ctx context.Context, a ActorStepAttempt, origin, commit string, in ActorDeliveryInput, receiverID string, authorize func(context.Context) bool) (ActorDeliveryIntent, error) {
	if !validActorDeliveryInput(in) || in.SourceKind != "step" || in.SwarmID != a.SwarmID || in.SourceID != strconv.FormatInt(a.Sequence, 10) || in.RequestID != a.RequestID || in.Commit != commit || in.ActorUID != a.Actor.UID || in.Incarnation != a.Actor.Incarnation || in.AtespaceUID != a.Actor.AtespaceUID {
		return ActorDeliveryIntent{}, ErrActorDeliveryConflict
	}
	var out ActorDeliveryIntent
	err := s.WithDurableTx(ctx, func(ctx context.Context) error {
		if authorize == nil || !authorize(ctx) {
			return ErrActorDeliveryDenied
		}
		if err := s.RecordActorStepAttestation(ctx, a, origin, commit); err != nil {
			return err
		}
		var err error
		out, err = s.prepareActorDelivery(ctx, in, authorize)
		if err != nil {
			return err
		}
		if receiverID != "" {
			digest := sha256.Sum256([]byte(in.Payload))
			target := ActorDeliveryInboxTarget{DeliveryID: out.ID, SwarmID: in.SwarmID, SourceKind: in.SourceKind, SourceID: in.SourceID, ActorUID: in.ActorUID, Incarnation: in.Incarnation, ReceiverID: receiverID, Digest: hex.EncodeToString(digest[:])}
			if err := s.selectActorDeliveryInbox(ctx, target, authorize); err != nil {
				return err
			}
		}
		return s.FinishActorOperation(ctx, a.Lease, true)
	})
	if err != nil {
		return ActorDeliveryIntent{}, err
	}
	return out, nil
}
