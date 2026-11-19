//go:build linux

package guest

import (
	"context"
	"errors"
	"sync"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
)

// ErrInboxHandoffUnavailable is deliberately not evidence of native delivery
// or absence. It must not permit a native fallback or a new server permit.
var ErrInboxHandoffUnavailable = errors.New("supervisor inbox handoff is unavailable")

// ErrInboxHandoffWaiting means a live authorized helper made no claim. This
// is the only helper result that permits another local query.
var ErrInboxHandoffWaiting = errors.New("supervisor inbox handoff has no live offer yet")

// InboxHandoff holds the single workflow instruction assigned to one trusted
// launch. The root supervisor constructs it under its launch lifecycle; it is
// not a public socket, capability advertisement, or source of server permits.
type InboxHandoff struct {
	mu       sync.Mutex
	launch   context.Context
	launchID string
	inbox    *deliverylog.Inbox
	grant    *deliverylog.InboxTicket
	terminal bool
}

// InboxWork is valid only for the trusted launch that successfully took it.
// The handoff is already durable before Payload can leave the supervisor.
type InboxWork struct {
	Ticket  deliverylog.InboxTicket
	Payload []byte
}

// NewInboxHandoff pins a supervisor-owned launch identity. Its context must be
// canceled when the launch is quiesced, replaced, or loses its server authority.
func NewInboxHandoff(launch context.Context, inbox *deliverylog.Inbox, launchID string) (*InboxHandoff, error) {
	if launch == nil || launch.Err() != nil || inbox == nil || !validHandoffID(launchID) {
		return nil, ErrInboxHandoffUnavailable
	}
	return &InboxHandoff{launch: launch, launchID: launchID, inbox: inbox}, nil
}

func validHandoffID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == ':' || c == '.') {
			return false
		}
	}
	return true
}

// Offer stores only an already reserved grant from the authenticated server.
// The caller must verify its authority; local helpers must not call Offer.
func (h *InboxHandoff) Offer(ctx context.Context, grant deliverylog.InboxTicket) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.terminal || h.launch.Err() != nil || ctx.Err() != nil || grant.LaunchID != h.launchID || !validHandoffID(grant.PermitID) || grant.Receipt.Scope != h.inbox.ActorScope() || grant.Receipt.InboxID != h.inbox.Identifier() {
		return ErrInboxHandoffUnavailable
	}
	if h.grant != nil {
		if *h.grant == grant {
			return nil
		}
		return ErrInboxHandoffUnavailable
	}
	receipt := grant.Receipt
	retained, err := h.inbox.Lookup(ctx, receipt.InboxID, receipt.DeliveryID, receipt.Digest)
	if err != nil || h.launch.Err() != nil || retained.Receipt != receipt || retained.State != deliverylog.InboxQueued {
		return ErrInboxHandoffUnavailable
	}
	h.grant = &grant
	return nil
}

// Take is allowed only for the exact authenticated consumer of this launch.
// The caller cannot use an actor-supplied launch string as authentication. Even
// a lost reply after a durable claim is not permission to retry the handoff.
func (h *InboxHandoff) Take(ctx context.Context, authenticatedLaunch string) (InboxWork, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.terminal || authenticatedLaunch != h.launchID || h.launch.Err() != nil || ctx.Err() != nil {
		return InboxWork{}, ErrInboxHandoffUnavailable
	}
	if h.grant == nil {
		return InboxWork{}, ErrInboxHandoffWaiting
	}
	grant := *h.grant
	h.grant = nil
	h.terminal = true
	claimCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(h.launch, cancel)
	defer stop()
	ticket, body, err := h.inbox.Claim(claimCtx, grant.Receipt, h.launchID, grant.PermitID)
	if err != nil || h.launch.Err() != nil || ctx.Err() != nil || ticket != grant {
		return InboxWork{}, ErrInboxHandoffUnavailable
	}
	return InboxWork{Ticket: ticket, Payload: body}, nil
}

// Revoke stops any later release from this launch. It cannot undo a prior claim.
func (h *InboxHandoff) Revoke() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.grant = nil
	h.terminal = true
}
