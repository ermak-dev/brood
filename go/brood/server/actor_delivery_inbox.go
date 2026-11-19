package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

const actorDeliveryInboxTimeout = 15 * time.Second

var errActorDeliveryInboxProtected = errors.New("native delivery is unavailable for a supervisor inbox selection")

type actorDeliveryInboxRelay interface {
	InboxReceiverID(string, int64) string
	DialInbox(context.Context, relay.InboxRequest) (net.Conn, error)
}

// deliverInbox is an explicit custody path for a retained intent. The user is
// the current approver for an answer or child; a step uses its workflow authority.
// A preexisting selection only allows status, even when the receipt is absent.
// Neither admission nor status is a native delivery outcome.
func (d *actorDeliveryService) deliverInbox(parent context.Context, deliveryID, userID string) (relay.InboxDisposition, error) {
	return d.deliverInboxMode(parent, deliveryID, userID, false)
}

// Initial admission for an already pinned inbox is called only by the owner
// of the freshly committed workspace attestation, never by a recovery worker.
func (d *actorDeliveryService) deliverInboxMode(parent context.Context, deliveryID, userID string, attestJustCommitted bool) (relay.InboxDisposition, error) {
	ctx, cancel := context.WithTimeout(parent, actorDeliveryInboxTimeout)
	defer cancel()
	s := d.server
	intent, err := s.store.GetActorDelivery(ctx, deliveryID)
	if err != nil {
		return "", err
	}
	if s.managed == nil {
		return "", relay.ErrInboxUnsupported
	}
	lockID, authorize := d.inboxAuthority(intent, userID)
	if authorize == nil {
		return "", store.ErrActorDeliveryConflict
	}
	unlock, err := d.lockDelivery(ctx, lockID)
	if err != nil {
		return "", err
	}
	defer unlock()

	selected, err := s.store.ActorDeliveryInboxStatus(ctx, intent.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	fresh := errors.Is(err, store.ErrNotFound)
	if intent.ProbeOnly || attestJustCommitted && (fresh || intent.Input.SourceKind != "step") {
		return "", store.ErrActorDeliveryConflict
	}
	target := inboxDeliveryTarget(intent, selected.Target.ReceiverID)
	if !fresh && target != selected.Target {
		return "", store.ErrActorDeliveryConflict
	}
	// Namespace lookup stays outside both the actor gates and the Store transaction.
	if err := d.authority(ctx, intent, authorize, false); err != nil {
		return "", err
	}
	if selected.Custody {
		if err := d.localAuthority(ctx, intent, authorize, false); err != nil {
			return "", err
		}
		return relay.InboxCustodyRecorded, nil
	}
	if selected.DispatchReserved {
		return "", store.ErrActorDeliveryConflict
	}
	actors, err := d.deliveryEffectActors(ctx, intent)
	if err != nil {
		return "", err
	}
	conn, request, err := d.openInbox(ctx, intent, target, fresh, attestJustCommitted, actors, authorize)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	response, err := relay.ReadInboxResponse(conn, request.Target)
	if err != nil {
		return "", err
	}
	if err := relay.ValidateInboxDisposition(request.Operation, response.Disposition); err != nil {
		return "", err
	}
	if response.Disposition != relay.InboxCustodyRecorded {
		return response.Disposition, nil
	}

	book, cancelBook := bookkeeping(ctx)
	defer cancelBook()
	target.ReceiverID = request.Target.ReceiverID
	if err := s.store.RecordActorDeliveryInboxCustody(book, target, store.ActorDeliveryInboxAdmitted, func(txctx context.Context) bool {
		return d.localAuthority(txctx, intent, authorize, false) == nil
	}); err != nil {
		return "", err
	}
	return response.Disposition, nil
}

func (d *actorDeliveryService) inboxAuthority(intent store.ActorDeliveryIntent, userID string) (string, func(context.Context) bool) {
	switch intent.Input.SourceKind {
	case "answer":
		// The native answer path locks by request ID.
		return intent.Input.SourceID, func(ctx context.Context) bool {
			return d.server.store.CheckActorDeliveryManager(ctx, intent.Input.SwarmID, userID) == nil
		}
	case "child":
		return intent.ID, func(ctx context.Context) bool { return d.childAuthority(ctx, userID, intent) }
	case "step":
		return intent.ID, func(ctx context.Context) bool { return d.instructionAuthority(ctx, intent.Input) }
	}
	return "", nil
}

// dispatchInbox arms one exact authorized launch after retained custody. The
// verifier must use trusted launch evidence, not a model claim or a cookie.
// It runs inside the same local authority transaction as the permit reservation.
func (d *actorDeliveryService) dispatchInbox(parent context.Context, deliveryID, userID, launchID string, verifyLaunch func(context.Context, store.ActorDeliveryInboxTarget, string) bool) (relay.InboxDisposition, error) {
	ctx, cancel := context.WithTimeout(parent, actorDeliveryInboxTimeout)
	defer cancel()
	s := d.server
	intent, err := s.store.GetActorDelivery(ctx, deliveryID)
	if err != nil {
		return "", err
	}
	if s.managed == nil || verifyLaunch == nil {
		return "", store.ErrActorDeliveryDenied
	}
	lockID, authorize := d.inboxAuthority(intent, userID)
	if authorize == nil {
		return "", store.ErrActorDeliveryConflict
	}
	unlock, err := d.lockDelivery(ctx, lockID)
	if err != nil {
		return "", err
	}
	defer unlock()
	selected, err := s.store.ActorDeliveryInboxStatus(ctx, deliveryID)
	if err != nil {
		return "", err
	}
	if !selected.Custody || selected.DispatchReserved || selected.Target != inboxDeliveryTarget(intent, selected.Target.ReceiverID) {
		return "", store.ErrActorDeliveryConflict
	}
	if err := d.authority(ctx, intent, authorize, false); err != nil {
		return "", err
	}
	actors, err := d.deliveryEffectActors(ctx, intent)
	if err != nil {
		return "", err
	}
	target := selected.Target
	wire := relay.InboxTarget{ActorUID: target.ActorUID, Incarnation: target.Incarnation, ReceiverID: target.ReceiverID, DeliveryID: target.DeliveryID, Digest: target.Digest}
	open := func() (net.Conn, error) {
		hub, ok := s.managed.hub.(actorDeliveryInboxRelay)
		if !ok {
			return nil, relay.ErrInboxUnsupported
		}
		release, err := s.managed.effects.acquire(ctx, actors...)
		if err != nil {
			return nil, err
		}
		defer release()
		if hub.InboxReceiverID(wire.ActorUID, wire.Incarnation) != wire.ReceiverID {
			return nil, relay.ErrInboxReceiverChanged
		}
		permit, err := s.store.ReserveActorDeliveryInboxDispatch(ctx, target, launchID, func(txctx context.Context, exact store.ActorDeliveryInboxTarget, launch string) bool {
			return d.localAuthority(txctx, intent, authorize, false) == nil && verifyLaunch(txctx, exact, launch)
		})
		if err != nil {
			return nil, err
		}
		// Even if the channel fails, the committed permit is not renewable.
		return hub.DialInbox(ctx, relay.InboxRequest{Operation: relay.InboxDispatch, Target: wire, Grant: &relay.InboxDispatchGrant{LaunchID: launchID, PermitID: permit}})
	}
	conn, err := open()
	if err != nil {
		return "", err
	}
	defer conn.Close()
	response, err := relay.ReadInboxResponse(conn, wire)
	if err != nil {
		return "", err
	}
	if err := relay.ValidateInboxDisposition(relay.InboxDispatch, response.Disposition); err != nil {
		return "", err
	}
	return response.Disposition, nil
}

func inboxDeliveryTarget(intent store.ActorDeliveryIntent, receiverID string) store.ActorDeliveryInboxTarget {
	in := intent.Input
	digest := sha256.Sum256([]byte(in.Payload))
	return store.ActorDeliveryInboxTarget{
		DeliveryID: intent.ID, SwarmID: in.SwarmID, SourceKind: in.SourceKind, SourceID: in.SourceID,
		ActorUID: in.ActorUID, Incarnation: in.Incarnation, ReceiverID: receiverID, Digest: hex.EncodeToString(digest[:]),
	}
}

// openInbox releases the same fences as native delivery as soon as the scoped
// channel opens. No response read or relay lookup runs inside a Store transaction.
func (d *actorDeliveryService) openInbox(ctx context.Context, intent store.ActorDeliveryIntent, target store.ActorDeliveryInboxTarget, fresh, attestJustCommitted bool, actors []string, authorize func(context.Context) bool) (net.Conn, relay.InboxRequest, error) {
	m := d.server.managed
	hub, ok := m.hub.(actorDeliveryInboxRelay)
	if !ok {
		return nil, relay.InboxRequest{}, relay.ErrInboxUnsupported
	}
	release, err := m.effects.acquire(ctx, actors...)
	if err != nil {
		return nil, relay.InboxRequest{}, err
	}
	defer release()
	if err := d.localAuthority(ctx, intent, authorize, false); err != nil {
		return nil, relay.InboxRequest{}, err
	}

	receiver := hub.InboxReceiverID(target.ActorUID, target.Incarnation)
	if receiver == "" {
		return nil, relay.InboxRequest{}, relay.ErrInboxUnsupported
	}
	if fresh {
		target.ReceiverID = receiver
	} else if target.ReceiverID != receiver {
		return nil, relay.InboxRequest{}, relay.ErrInboxReceiverChanged
	}
	request := relay.InboxRequest{Operation: relay.InboxStatus, Target: relay.InboxTarget{
		ActorUID: target.ActorUID, Incarnation: target.Incarnation, ReceiverID: target.ReceiverID,
		DeliveryID: target.DeliveryID, Digest: target.Digest,
	}}
	if fresh || attestJustCommitted {
		request.Operation = relay.InboxAdmit
		request.Payload = json.RawMessage(intent.Input.Payload)
		if _, err := relay.EncodeInboxRequest(request); err != nil {
			return nil, relay.InboxRequest{}, err
		}
	}
	if fresh {
		if err := d.server.store.SelectActorDeliveryInbox(ctx, target, func(txctx context.Context) bool {
			return d.localAuthority(txctx, intent, authorize, false) == nil
		}); err != nil {
			return nil, relay.InboxRequest{}, err
		}
	}
	// Recheck the source at open, after peer observation and the durable commit.
	// DialInbox reauthenticates the actor/incarnation and checks the exact receiver.
	if err := d.localAuthority(ctx, intent, authorize, false); err != nil {
		return nil, relay.InboxRequest{}, err
	}
	conn, err := hub.DialInbox(ctx, request)
	return conn, request, err
}
