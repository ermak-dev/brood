package relay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// InboxOperation is distinct from the legacy native-session request. Status
// never carries a message body or gives permission to submit a fresh turn.
type InboxOperation string

const (
	InboxAdmit    InboxOperation = "admit"
	InboxStatus   InboxOperation = "status"
	InboxDispatch InboxOperation = "dispatch"
)

// InboxTarget identifies one exact instruction and retained supervisor inbox.
// Transport users must independently authenticate the current actor connection.
type InboxTarget struct {
	ActorUID    string `json:"actorUid"`
	Incarnation int64  `json:"incarnation"`
	ReceiverID  string `json:"receiverId"`
	DeliveryID  string `json:"deliveryId"`
	Digest      string `json:"digest"`
}

// InboxDispatchGrant binds a previously reserved server permit to one launch.
// Only the authenticated server can present it; it is never a local helper input.
type InboxDispatchGrant struct {
	LaunchID string `json:"launchId"`
	PermitID string `json:"permitId"`
}

// InboxRequest carries canonical JSON on admission, only identity on lookup,
// and an existing permit on dispatch. It is not a routed peer-message frame.
type InboxRequest struct {
	Operation InboxOperation      `json:"operation"`
	Target    InboxTarget         `json:"target"`
	Payload   json.RawMessage     `json:"payload,omitempty"`
	Grant     *InboxDispatchGrant `json:"grant,omitempty"`
}

// InboxDisposition describes only receiving-side custody. In particular, an
// absent or unavailable observation does not authorize any other delivery.
type InboxDisposition string

const (
	InboxCustodyRecorded InboxDisposition = "admitted_to_supervisor_inbox"
	InboxReceiptAbsent   InboxDisposition = "receipt_absent"
	InboxReceiptUnknown  InboxDisposition = "receipt_unavailable"
	InboxDispatchArmed   InboxDisposition = "dispatch_armed"
)

// InboxResponse deliberately contains no OK, Duplicate, or Verdict fields.
// It must not be interpreted by a legacy native-session delivery client.
type InboxResponse struct {
	Disposition InboxDisposition `json:"disposition"`
	Target      InboxTarget      `json:"target"`
}

var errInboxFrame = errors.New("invalid supervisor inbox frame")

func validInboxWireID(value string) bool {
	if len(value) == 0 || len(value) > 128 || value[0] == '.' {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-' || c == ':' || c == '.') {
			return false
		}
	}
	return true
}

func validInboxTarget(target InboxTarget) bool {
	return validInboxWireID(target.ActorUID) && target.Incarnation > 0 && validMessageJournalID(target.ReceiverID) &&
		validInboxWireID(target.DeliveryID) && validMessageJournalID(target.Digest)
}

func inboxFrameDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// EncodeInboxRequest encodes one bounded canonical frame. Noncanonical payloads
// are rejected rather than normalized after binding the request digest.
func EncodeInboxRequest(request InboxRequest) ([]byte, error) {
	if !validInboxTarget(request.Target) || len(request.Payload) > MaxMessageBytes {
		return nil, errInboxFrame
	}
	switch request.Operation {
	case InboxAdmit:
		if request.Grant != nil || len(request.Payload) == 0 || request.Payload[0] != '{' || inboxFrameDigest(request.Payload) != request.Target.Digest {
			return nil, errInboxFrame
		}
		var compact bytes.Buffer
		if json.Compact(&compact, request.Payload) != nil || !bytes.Equal(compact.Bytes(), request.Payload) {
			return nil, errInboxFrame
		}
	case InboxStatus:
		if request.Grant != nil || len(request.Payload) != 0 {
			return nil, errInboxFrame
		}
	case InboxDispatch:
		if len(request.Payload) != 0 || request.Grant == nil || !validInboxWireID(request.Grant.LaunchID) || !validInboxWireID(request.Grant.PermitID) {
			return nil, errInboxFrame
		}
	default:
		return nil, errInboxFrame
	}
	frame, err := json.Marshal(request)
	if err != nil || len(frame) > MaxMessageBytes {
		return nil, errInboxFrame
	}
	// Marshal can HTML-escape a RawMessage. Verify the canonical payload is
	// exactly the sequence the receiver will hash and retain.
	if request.Operation == InboxAdmit {
		var wire InboxRequest
		if json.Unmarshal(frame, &wire) != nil || !bytes.Equal(wire.Payload, request.Payload) {
			return nil, errInboxFrame
		}
	}
	return frame, nil
}

// DecodeInboxRequest rejects field aliases, duplicates, unknowns and trailing
// bytes by requiring the exact envelope spelling emitted by the encoder.
func DecodeInboxRequest(frame []byte) (InboxRequest, error) {
	var request InboxRequest
	if len(frame) > MaxMessageBytes || json.Unmarshal(frame, &request) != nil {
		return InboxRequest{}, errInboxFrame
	}
	canonical, err := EncodeInboxRequest(request)
	if err != nil || !bytes.Equal(frame, canonical) {
		return InboxRequest{}, errInboxFrame
	}
	return request, nil
}

// EncodeInboxResponse returns the canonical custody or read-only status frame.
func EncodeInboxResponse(response InboxResponse) ([]byte, error) {
	if !validInboxTarget(response.Target) {
		return nil, errInboxFrame
	}
	switch response.Disposition {
	case InboxCustodyRecorded, InboxReceiptAbsent, InboxReceiptUnknown, InboxDispatchArmed:
		return json.Marshal(response)
	default:
		return nil, errInboxFrame
	}
}

// ValidateInboxDisposition distinguishes a positive custody receipt from a
// positive offer. Neither proves admission into the downstream model session.
func ValidateInboxDisposition(operation InboxOperation, disposition InboxDisposition) error {
	switch operation {
	case InboxAdmit:
		if disposition == InboxCustodyRecorded || disposition == InboxReceiptUnknown {
			return nil
		}
	case InboxStatus:
		if disposition == InboxCustodyRecorded || disposition == InboxReceiptAbsent || disposition == InboxReceiptUnknown {
			return nil
		}
	case InboxDispatch:
		if disposition == InboxDispatchArmed || disposition == InboxReceiptUnknown {
			return nil
		}
	}
	return errInboxFrame
}

// DecodeInboxResponse verifies the entire retained identity against the request.
func DecodeInboxResponse(frame []byte, expected InboxTarget) (InboxResponse, error) {
	var response InboxResponse
	if len(frame) > MaxMessageBytes || json.Unmarshal(frame, &response) != nil || response.Target != expected {
		return InboxResponse{}, errInboxFrame
	}
	canonical, err := EncodeInboxResponse(response)
	if err != nil || !bytes.Equal(frame, canonical) {
		return InboxResponse{}, errInboxFrame
	}
	return response, nil
}
