//go:build linux

package guest

import (
	"context"
	"errors"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

// SupervisorInboxEndpoint handles bounded custody requests from a separately
// authenticated server relay. It does not claim a payload, submit a prompt,
// or return any legacy native delivery or idle signal.
func SupervisorInboxEndpoint(inbox *deliverylog.Inbox) func(context.Context, []byte) ([]byte, error) {
	return SupervisorInboxWithDispatch(inbox, nil)
}

// SupervisorInboxWithDispatch adds a server-grant path to an existing custody
// endpoint. Resolve must obtain the current launch from supervisor-owned state;
// a grant's launch field only selects it and does not authenticate a process.
func SupervisorInboxWithDispatch(inbox *deliverylog.Inbox, resolve func(context.Context, string) *InboxHandoff) func(context.Context, []byte) ([]byte, error) {
	return func(ctx context.Context, frame []byte) ([]byte, error) {
		request, err := relay.DecodeInboxRequest(frame)
		if err != nil {
			return nil, err
		}
		target := request.Target
		scope := deliverylog.Scope{ActorUID: target.ActorUID, Incarnation: target.Incarnation}
		if inbox == nil || scope != inbox.ActorScope() || target.ReceiverID != inbox.Identifier() {
			return nil, deliverylog.ErrScope
		}
		disposition := relay.InboxReceiptUnknown
		expected := deliverylog.InboxReceipt{Scope: scope, InboxID: target.ReceiverID, DeliveryID: target.DeliveryID, Digest: target.Digest}
		if request.Operation == relay.InboxDispatch {
			grant := request.Grant
			if resolve != nil && ctx.Err() == nil {
				handoff := resolve(ctx, grant.LaunchID)
				if handoff != nil && handoff.inbox == inbox && handoff.Offer(ctx, deliverylog.InboxTicket{Receipt: expected, LaunchID: grant.LaunchID, PermitID: grant.PermitID}) == nil {
					disposition = relay.InboxDispatchArmed
				}
			}
			return relay.EncodeInboxResponse(relay.InboxResponse{Target: target, Disposition: disposition})
		}
		var receipt deliverylog.InboxReceipt
		switch request.Operation {
		case relay.InboxAdmit:
			receipt, err = inbox.Enqueue(ctx, target.ReceiverID, target.DeliveryID, request.Payload)
		case relay.InboxStatus:
			status, lookupErr := inbox.Lookup(ctx, target.ReceiverID, target.DeliveryID, target.Digest)
			receipt, err = status.Receipt, lookupErr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil && receipt == expected {
			disposition = relay.InboxCustodyRecorded
		} else if request.Operation == relay.InboxStatus && errors.Is(err, deliverylog.ErrInboxMissing) {
			disposition = relay.InboxReceiptAbsent
		}
		return relay.EncodeInboxResponse(relay.InboxResponse{Target: target, Disposition: disposition})
	}
}
