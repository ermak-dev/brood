package relay

// FEATURE: Явная квитанция чтения completion acceptance до quiescence (#290).
// SPEC: Строгая bounded квитанция; служебный ACK допустим только root relay,
// actor-facing decoder по-прежнему отказывает ему; legacy ответы без квитанции прежние.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testCompletionReceipt() CompletionReceipt {
	return CompletionReceipt{Version: 1, SwarmID: "swarm", Pass: 1, Sequence: 2, RequestID: "delivery-2", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
}

func TestCompletionReceiptRoundtripAndSupervisorOnlyAck(t *testing.T) {
	receipt := testCompletionReceipt()
	data, err := WithCompletionReceipt(nil, receipt)
	if err != nil {
		t.Fatal(err)
	}
	got, present, err := ReadCompletionReceipt(data)
	if err != nil || !present || got != receipt {
		t.Fatalf("receipt roundtrip: %+v %v %v", got, present, err)
	}
	request, err := CompletionReceiptAck(receipt)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := DecodeSwarmRequest(wire); ok {
		t.Fatal("actor decoder accepted supervisor ACK")
	}
	decoded, ok := decodeSupervisorSwarmRequest(wire)
	if !ok || !reflect.DeepEqual(decoded, request) {
		t.Fatal("supervisor ACK rejected")
	}
	ack, err := DecodeCompletionReceiptAck(decoded)
	if err != nil || ack != receipt {
		t.Fatal("ACK changed scope")
	}
	legacy := json.RawMessage(`{"children":[]}`)
	preserved, err := WithCompletionReceipt(legacy, receipt)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(preserved, &fields) != nil || string(fields["children"]) != "[]" {
		t.Fatal("receipt erased business data")
	}
	if _, present, err := ReadCompletionReceipt(legacy); err != nil || present {
		t.Fatal("legacy data invented receipt")
	}
}

func TestCompletionReceiptRejectsAmbiguityAndUnsafeScope(t *testing.T) {
	receipt := testCompletionReceipt()
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{
		strings.Replace(string(raw), `"version":1`, `"version":2`, 1),
		strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(string(raw), `"pass":1`, `"Pass":1`, 1),
		strings.Replace(string(raw), `"sequence":2`, `"sequence":0`, 1),
		strings.Replace(string(raw), `"nonce":"`+receipt.Nonce+`"`, `"nonce":null`, 1),
		strings.Replace(string(raw), `"request_id":"delivery-2"`, `"request_id":"bad\u0000id"`, 1),
		strings.Replace(string(raw), `"pass":1,`, "", 1),
	} {
		if _, _, err := ReadCompletionReceipt([]byte(`{"completion_receipt":` + changed + `}`)); err == nil {
			t.Fatal("invalid receipt accepted")
		}
	}
	if _, err := WithCompletionReceipt(json.RawMessage(`{"completion_receipt":{}}`), receipt); err == nil {
		t.Fatal("existing receipt silently replaced")
	}
	if _, err := WithCompletionReceipt(json.RawMessage(`{"children":[],"children":[1]}`), receipt); err == nil {
		t.Fatal("ambiguous business data accepted")
	}
	request, err := CompletionReceiptAck(receipt)
	if err != nil {
		t.Fatal(err)
	}
	request.To = "foreign"
	wire, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeSupervisorSwarmRequest(wire); ok {
		t.Fatal("ACK with forged envelope target accepted")
	}
}

func TestCompletionReceiptAuthenticatedTransport(t *testing.T) {
	f := setup(t)
	calls := make(chan SwarmCaller, 1)
	f.cfg.OnSwarmSession = func(ctx context.Context, call SwarmCaller) { calls <- call; <-ctx.Done() }
	receipt := testCompletionReceipt()
	f.h.SetSwarmHandler(func(ctx context.Context, uid string, generation int64, req SwarmRequest) SwarmResponse {
		got, err := DecodeCompletionReceiptAck(req)
		if uid != "actor-uid" || generation != 3 || err != nil || got != receipt {
			t.Errorf("wrong authenticated ACK: %s/%d %+v %v", uid, generation, got, err)
			return SwarmResponse{}
		}
		return SwarmResponse{OK: true, Text: "receipt observed"}
	})
	start(t, f, f.cfg)
	var call SwarmCaller
	select {
	case call = <-calls:
	case <-time.After(3 * time.Second):
		t.Fatal("no session caller")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := CompletionReceiptAck(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got := call(ctx, request); !got.OK || got.Text != "receipt observed" {
		t.Fatalf("authenticated receipt refused: %+v", got)
	}
	f.current.Store(false)
	if got := call(ctx, request); got.OK {
		t.Fatal("revoked peer acknowledged completion")
	}
}

func TestCompletionReceiptLocalAckIsExactAndBounded(t *testing.T) {
	receipt := testCompletionReceipt()
	frame, err := CompletionReceiptFrame(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidCompletionReceiptFrame(frame, receipt) {
		t.Fatal("valid local receipt rejected")
	}
	for _, data := range []string{`{}`, `{"nonce":"other"}`, `{"nonce":"` + receipt.Nonce + `","nonce":"other"}`, string(frame) + ` {}`, strings.Repeat("x", 1025)} {
		if ValidCompletionReceiptFrame([]byte(data), receipt) {
			t.Fatal("invalid local ACK accepted")
		}
	}
}
