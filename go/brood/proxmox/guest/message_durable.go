//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"time"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

// NewDurableMessageHandler binds delivery receipts to an already opened private
// journal. The caller owns its lifetime and capability advertisement separately.
// Journal metadata is never added to the closed Claude Code envelope grammar.
func NewDurableMessageHandler(journal *deliverylog.Journal) func(context.Context, relay.MessageRequest) relay.MessageResponse {
	handler := durableMessageHandler{journal: journal, supervisor: func() bool { return os.Getuid() == 0 && os.Geteuid() == 0 }, transport: DeliverMessage}
	return handler.receive
}

type durableMessageHandler struct {
	journal    *deliverylog.Journal
	supervisor func() bool
	transport  func(context.Context, relay.MessageRequest) relay.MessageResponse
}

func durableMessageUnknown(reason string) relay.MessageResponse {
	return relay.MessageResponse{Verdict: reason}
}

func durableMessageDuplicate(waitSec int) relay.MessageResponse {
	verdict := "duplicate; prior delivery confirmed"
	if waitSec > 0 {
		verdict += "; idle status not rechecked"
	}
	return relay.MessageResponse{OK: true, Duplicate: true, Verdict: verdict}
}

func (h durableMessageHandler) receive(ctx context.Context, req relay.MessageRequest) relay.MessageResponse {
	if h.supervisor == nil || !h.supervisor() {
		return durableMessageUnknown("durable delivery requires guest supervisor")
	}
	if h.transport == nil {
		return durableMessageUnknown("message transport unavailable")
	}
	if req.DeliveryID == "" {
		if req.JournalID != "" || req.Replay {
			return durableMessageUnknown("invalid durable delivery metadata")
		}
		return h.transport(ctx, req)
	}
	if h.journal == nil {
		return durableMessageUnknown("durable delivery journal unavailable")
	}
	if h.journal.Identifier() == "" || req.JournalID != h.journal.Identifier() {
		return durableMessageUnknown("delivery journal identity changed or is unavailable; no message was resent")
	}
	canonical := req
	canonical.Replay = false
	payload, err := json.Marshal(canonical)
	if err != nil || len(payload) > relay.MaxMessageBytes {
		return durableMessageUnknown("invalid durable message request")
	}
	helper := req
	helper.DeliveryID = ""
	helper.JournalID = ""
	helper.Replay = false
	helperData, err := json.Marshal(helper)
	if err != nil {
		return durableMessageUnknown("invalid durable message request")
	}
	if _, err := ReadDeliveryRequest(bytes.NewReader(helperData)); err != nil {
		return durableMessageUnknown("invalid durable message request")
	}
	if req.Replay {
		state, err := h.journal.Lookup(ctx, req.DeliveryID, payload)
		if err != nil {
			return durableMessageUnknown("durable delivery status unavailable: " + err.Error())
		}
		switch state {
		case deliverylog.Duplicate:
			return durableMessageDuplicate(req.WaitSec)
		default:
			// Старая not-sent квитанция не исключает позднюю попытку, утраченную при rollback.
			return durableMessageUnknown("retained journal has no confirmed delivery; no message was resent")
		}
	}
	admission, err := h.journal.Begin(ctx, req.DeliveryID, payload)
	if err != nil {
		return durableMessageUnknown("durable delivery was not admitted: " + err.Error())
	}
	switch admission.State {
	case deliverylog.Duplicate:
		return durableMessageDuplicate(req.WaitSec)
	case deliverylog.Unknown:
		return durableMessageUnknown("delivery remains unconfirmed; retained history forbids a resend")
	case deliverylog.New:
	default:
		return durableMessageUnknown("invalid delivery journal admission")
	}
	result := h.transport(ctx, helper)
	outcome := deliverylog.Unconfirmed
	if result.Verdict == "" || result.Duplicate || result.OK && result.NotSent {
		result = durableMessageUnknown("recipient returned contradictory delivery evidence")
	} else if result.OK {
		outcome = deliverylog.Delivered
	} else if result.NotSent {
		outcome = deliverylog.NotSent
	}
	// Сохранение уже наблюдённого исхода переживает отмену входящего канала.
	settle, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.journal.Finish(settle, admission.Ticket, outcome); err != nil {
		return durableMessageUnknown("delivery receipt persistence failed; delivery remains unconfirmed")
	}
	return result
}
