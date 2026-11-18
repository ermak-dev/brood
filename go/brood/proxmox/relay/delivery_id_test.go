package relay

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDeliveryMetadataIsAbsentFromLegacyWireFrames(t *testing.T) {
	req := MessageRequest{To: "worker", Body: "instruction", From: "swarm:one"}
	data, err := encodeMessageFrame(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"to":"worker","body":"instruction","from":"swarm:one","waitSec":0}` {
		t.Fatalf("legacy request wire changed: %s", data)
	}
	response, err := json.Marshal(MessageResponse{OK: true, Verdict: "delivered"})
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != `{"ok":true,"verdict":"delivered"}` {
		t.Fatalf("legacy response wire changed: %s", response)
	}
	var legacy MessageResponse
	if err := json.Unmarshal([]byte(`{"ok":true,"verdict":"delivered"}`), &legacy); err != nil || legacy.Duplicate {
		t.Fatal("old receipt inferred duplicate support")
	}
}

func TestDeliveryMetadataRequiresPairedCanonicalJournalIdentity(t *testing.T) {
	for _, req := range []MessageRequest{
		{DeliveryID: "stable"}, {JournalID: strings.Repeat("a", 64)}, {Replay: true},
		{DeliveryID: "stable", JournalID: strings.Repeat("A", 64)},
		{DeliveryID: "stable", JournalID: "short"},
	} {
		req.To, req.From, req.Body = "worker", "owner", "work"
		if _, err := encodeMessageFrame(req); err == nil {
			t.Errorf("encoder accepted unbound metadata: %+v", req)
		}
		frame, _ := json.Marshal(req)
		if _, err := decodeMessageFrame(frame); err == nil {
			t.Errorf("decoder accepted unbound metadata: %+v", req)
		}
	}
}

func TestDeliveryMetadataRoundTripsWithinExistingFrameBound(t *testing.T) {
	req := MessageRequest{JournalID: strings.Repeat("a", 64), Replay: true, DeliveryID: "step:stable-identity", To: "worker", Body: "instruction", From: "swarm:one", WaitSec: 0}
	frame, err := encodeMessageFrame(req)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMessageFrame(frame)
	if err != nil || !reflect.DeepEqual(decoded, req) {
		t.Fatalf("delivery ID lost scope: %+v %v", decoded, err)
	}
	response := MessageResponse{OK: true, Duplicate: true, Verdict: "duplicate; prior delivery confirmed"}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var got MessageResponse
	if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, response) {
		t.Fatalf("duplicate receipt lost: %+v %v", got, err)
	}
	req.DeliveryID = strings.Repeat("x", MaxMessageBytes)
	if _, err := encodeMessageFrame(req); err == nil {
		t.Fatal("delivery metadata bypassed message frame size limit")
	}
}
