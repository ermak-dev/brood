package relay

import (
	"errors"
	"net/http"
)

const messageProtocolHeader = "X-Brood-Message-Protocol"
const messageJournalHeader = "X-Brood-Delivery-Journal"
const messageDedupProtocol = "dedup-v2"

// ErrMessageDedupUnsupported refuses a stable-ID frame before its channel opens.
// It does not resolve uncertainty from a preceding delivery on another connection.
var ErrMessageDedupUnsupported = errors.New("guest relay does not support durable message delivery")

// ErrDeliveryJournalChanged preserves uncertainty from the previous journal.
// A new journal is not proof that an earlier instruction was never delivered.
var ErrDeliveryJournalChanged = errors.New("guest delivery journal identity changed")

func validMessageJournalID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func messageDedupAdvertisement(header http.Header) (string, error) {
	values := header.Values(messageProtocolHeader)
	journals := header.Values(messageJournalHeader)
	if len(values) == 0 && len(journals) == 0 {
		return "", nil
	}
	if len(values) != 1 || values[0] != messageDedupProtocol || len(journals) != 1 || !validMessageJournalID(journals[0]) {
		return "", errors.New("invalid message delivery protocol advertisement")
	}
	return journals[0], nil
}

// DeliveryJournalID reports only the current authenticated incarnation's journal.
// DialMessage rechecks the selected connection; this observation is not permission
// to repeat an uncertain instruction against a new journal.
func (h *Hub) DeliveryJournalID(uid string, generation int64) string {
	p := h.session(uid, generation)
	if p == nil {
		return ""
	}
	return p.messageJournalID
}

// SupportsMessageDeduplication requires both a durable protocol and journal identity.
func (h *Hub) SupportsMessageDeduplication(uid string, generation int64) bool {
	return h.DeliveryJournalID(uid, generation) != ""
}
