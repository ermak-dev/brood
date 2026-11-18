package relay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// CompletionReceiptAckTool is supervisor-only, not an actor command or MCP tool.
const CompletionReceiptAckTool = "completion_receipt_ack"

// MaxCompletionReceiptFrame bounds the local client's fixed acknowledgement frame.
const MaxCompletionReceiptFrame = 256

// CompletionReceipt binds acceptance acknowledgement to one immutable completion intent.
// Nonce is a server-generated unpredictable receipt, not actor execution authority.
type CompletionReceipt struct {
	Version   int    `json:"version"`
	SwarmID   string `json:"swarm_id"`
	Pass      int64  `json:"pass"`
	Sequence  int64  `json:"sequence"`
	RequestID string `json:"request_id"`
	Nonce     string `json:"nonce"`
}

func receiptID(value string, attempt bool) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || attempt && (c == '.' || c == ':') {
			continue
		}
		return false
	}
	return true
}

// Validate rejects unsupported versions, ambiguous scope and noncanonical receipt nonces.
func (r CompletionReceipt) Validate() error {
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(r.Nonce)
	if r.Version != 1 || !receiptID(r.SwarmID, false) || !receiptID(r.RequestID, true) || r.Pass < 1 || r.Sequence < r.Pass || err != nil || len(nonce) != 32 {
		return errors.New("invalid completion receipt")
	}
	return nil
}

func receiptFields(raw []byte) (CompletionReceipt, error) {
	fail := errors.New("malformed completion receipt")
	if len(raw) > 1024 || !utf8.Valid(raw) || !uniqueSwarmFields(raw) {
		return CompletionReceipt{}, fail
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 6 {
		return CompletionReceipt{}, fail
	}
	for _, name := range []string{"version", "swarm_id", "pass", "sequence", "request_id", "nonce"} {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return CompletionReceipt{}, fail
		}
	}
	var result CompletionReceipt
	if json.Unmarshal(raw, &result) != nil || result.Validate() != nil {
		return CompletionReceipt{}, fail
	}
	return result, nil
}

// WithCompletionReceipt adds one receipt without discarding existing object-shaped data.
// A previously present receipt is a conflict, never silently overwritten.
func WithCompletionReceipt(data json.RawMessage, receipt CompletionReceipt) (json.RawMessage, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if len(data) > 0 && !bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if len(data) > MaxMessageBytes || !utf8.Valid(data) || !uniqueSwarmFields(data) || json.Unmarshal(data, &fields) != nil {
			return nil, errors.New("invalid completion response data")
		}
	}
	if _, exists := fields["completion_receipt"]; exists {
		return nil, errors.New("completion response already has a receipt")
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	fields["completion_receipt"] = raw
	encoded, err := json.Marshal(fields)
	if err != nil || len(encoded) > MaxMessageBytes {
		return nil, errors.New("completion response exceeds size limit")
	}
	return encoded, nil
}

// ReadCompletionReceipt reads optional metadata without inventing it for legacy replies.
func ReadCompletionReceipt(data json.RawMessage) (CompletionReceipt, bool, error) {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return CompletionReceipt{}, false, nil
	}
	if len(data) > MaxMessageBytes || !utf8.Valid(data) || !uniqueSwarmFields(data) {
		return CompletionReceipt{}, false, errors.New("invalid completion response data")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return CompletionReceipt{}, false, errors.New("invalid completion response data")
	}
	raw, exists := fields["completion_receipt"]
	if !exists {
		return CompletionReceipt{}, false, nil
	}
	receipt, err := receiptFields(raw)
	return receipt, true, err
}

// CompletionReceiptAck constructs the root-only authenticated relay acknowledgement.
func CompletionReceiptAck(receipt CompletionReceipt) (SwarmRequest, error) {
	if err := receipt.Validate(); err != nil {
		return SwarmRequest{}, err
	}
	raw, err := json.Marshal(receipt)
	return SwarmRequest{Tool: CompletionReceiptAckTool, Arguments: raw}, err
}

// DecodeCompletionReceiptAck rejects identity/target injection and malformed receipts.
func DecodeCompletionReceiptAck(request SwarmRequest) (CompletionReceipt, error) {
	if request.Tool != CompletionReceiptAckTool || request.To != "" || request.Message != "" || request.WaitSeconds != 0 {
		return CompletionReceipt{}, errors.New("invalid supervisor completion acknowledgement")
	}
	return receiptFields(request.Arguments)
}

// CompletionReceiptFrame is emitted by the local client only after response decoding.
func CompletionReceiptFrame(receipt CompletionReceipt) ([]byte, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Nonce string `json:"nonce"`
	}{Nonce: receipt.Nonce})
}

// ValidCompletionReceiptFrame checks the client's exact bounded acknowledgement.
func ValidCompletionReceiptFrame(data []byte, receipt CompletionReceipt) bool {
	if receipt.Validate() != nil || len(data) > MaxCompletionReceiptFrame || !utf8.Valid(data) || !uniqueSwarmFields(data) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 1 {
		return false
	}
	var nonce string
	return json.Unmarshal(fields["nonce"], &nonce) == nil && nonce == receipt.Nonce
}

// CompletionReceiptForResponse accepts metadata only for the exact completed request.
// Unrelated tools, malformed metadata and legacy replies never trigger an acknowledgement.
func CompletionReceiptForResponse(request SwarmRequest, response SwarmResponse) (CompletionReceipt, bool) {
	if !response.OK || response.Outcome != "accepted" || request.Tool != "pipeline_visit_complete" && request.Tool != "child_join" {
		return CompletionReceipt{}, false
	}
	receipt, present, err := ReadCompletionReceipt(response.Data)
	if err != nil || !present {
		return CompletionReceipt{}, false
	}
	var args struct {
		RunID           string `json:"run_id"`
		Sequence        int64  `json:"sequence"`
		RequestID       string `json:"request_id"`
		ParentSequence  int64  `json:"parent_sequence"`
		ParentRequestID string `json:"parent_request_id"`
	}
	if json.Unmarshal(request.Arguments, &args) != nil {
		return CompletionReceipt{}, false
	}
	if request.Tool == "pipeline_visit_complete" {
		if receipt.SwarmID != args.RunID || receipt.Sequence != args.Sequence || receipt.RequestID != args.RequestID {
			return CompletionReceipt{}, false
		}
	} else if receipt.Sequence != args.ParentSequence || receipt.RequestID != args.ParentRequestID {
		return CompletionReceipt{}, false
	}
	return receipt, true
}

func validSupervisorSwarmRequest(request SwarmRequest) bool {
	if validSwarmRequest(request) {
		return true
	}
	_, err := DecodeCompletionReceiptAck(request)
	return err == nil
}

func decodeSupervisorSwarmRequest(data []byte) (SwarmRequest, bool) {
	if request, ok := DecodeSwarmRequest(data); ok {
		return request, true
	}
	if len(data) > MaxMessageBytes || !utf8.Valid(data) || !uniqueSwarmFields(data) {
		return SwarmRequest{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 2 || fields["tool"] == nil || fields["arguments"] == nil {
		return SwarmRequest{}, false
	}
	var request SwarmRequest
	if json.Unmarshal(data, &request) != nil {
		return SwarmRequest{}, false
	}
	_, err := DecodeCompletionReceiptAck(request)
	return request, err == nil
}
