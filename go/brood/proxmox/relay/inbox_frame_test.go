package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func inboxWireRequest() InboxRequest {
	return InboxRequest{Operation: InboxAdmit, Target: InboxTarget{
		ActorUID: "actor-1", Incarnation: 3, ReceiverID: strings.Repeat("a", 64),
		DeliveryID: "pass-1:step-2", Digest: inboxFrameDigest([]byte(`{"body":"exact prompt"}`)),
	}, Payload: json.RawMessage(`{"body":"exact prompt"}`)}
}

func TestInboxFramesBindExactCustodyNotLegacyDelivery(t *testing.T) {
	request := inboxWireRequest()
	frame, err := EncodeInboxRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeInboxRequest(frame)
	if err != nil || got.Target != request.Target || got.Operation != InboxAdmit || !bytes.Equal(got.Payload, request.Payload) {
		t.Fatalf("request changed at the receiver: %v", err)
	}
	response := InboxResponse{Disposition: InboxCustodyRecorded, Target: request.Target}
	frame, err = EncodeInboxResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeInboxResponse(frame, request.Target); err != nil || got != response {
		t.Fatalf("exact custody receipt not recovered: %+v %v", got, err)
	}
	if _, err := ReadMessageResponse(bytes.NewReader(frame)); err == nil {
		t.Fatal("custody receipt is accepted as a legacy native message verdict")
	}
	foreign := request.Target
	foreign.Incarnation++
	if _, err := DecodeInboxResponse(frame, foreign); err == nil {
		t.Fatal("foreign incarnation obtained the receipt")
	}
}

func TestInboxRelayPinsAdvertisedReceiverAndNeverFallsBack(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	var native, custody atomic.Int32
	cfg.HandleMessage = func(context.Context, MessageRequest) MessageResponse {
		native.Add(1)
		return MessageResponse{OK: true, Verdict: "legacy"}
	}
	target := inboxWireRequest().Target
	target.ActorUID = "actor-uid"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status := InboxRequest{Operation: InboxStatus, Target: target}
	stopLegacy := start(t, f, cfg)
	original := f.h.session(target.ActorUID, target.Incarnation)
	if _, err := f.h.CallInbox(ctx, status); !errors.Is(err, ErrInboxUnsupported) || native.Load() != 0 {
		t.Fatalf("legacy peer received a custody request: %v", err)
	}
	cfg.InboxReceiverID = target.ReceiverID
	cfg.HandleInbox = func(_ context.Context, raw []byte) ([]byte, error) {
		request, err := DecodeInboxRequest(raw)
		if err != nil {
			return nil, err
		}
		custody.Add(1)
		return EncodeInboxResponse(InboxResponse{Disposition: InboxCustodyRecorded, Target: request.Target})
	}
	start(t, f, cfg)
	await(t, func() bool {
		return f.h.session(target.ActorUID, target.Incarnation) != original && f.h.InboxReceiverID(target.ActorUID, target.Incarnation) == target.ReceiverID
	})
	stopLegacy()
	if receipt, err := f.h.CallInbox(ctx, status); err != nil || receipt.Disposition != InboxCustodyRecorded || custody.Load() != 1 {
		t.Fatalf("authenticated peer did not return custody: %+v %v", receipt, err)
	}
	foreign := status
	foreign.Target.ReceiverID = strings.Repeat("b", 64)
	if _, err := f.h.CallInbox(ctx, foreign); !errors.Is(err, ErrInboxReceiverChanged) || custody.Load() != 1 || native.Load() != 0 {
		t.Fatalf("wrong receiver obtained an effect or fell back to native: %v", err)
	}
	f.current.Store(false)
	if _, err := f.h.CallInbox(ctx, status); err == nil || custody.Load() != 1 {
		t.Fatalf("revoked peer handled another request: %v", err)
	}
}

func TestInboxDispatchFramesCannotCarryAnotherPromptOrReceipt(t *testing.T) {
	admit := inboxWireRequest()
	grant := InboxDispatchGrant{LaunchID: "launch-1", PermitID: "issued-once"}
	request := InboxRequest{Operation: InboxDispatch, Target: admit.Target, Grant: &grant}
	frame, err := EncodeInboxRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeInboxRequest(frame)
	if err != nil || got.Operation != InboxDispatch || got.Target != request.Target || got.Grant == nil || *got.Grant != grant || len(got.Payload) != 0 {
		t.Fatalf("permit changed in transit: %+v %v", got, err)
	}
	inconsistent := request
	inconsistent.Payload = admit.Payload
	if _, err := EncodeInboxRequest(inconsistent); err == nil {
		t.Fatal("dispatch can carry new prompt bytes")
	}
	inconsistent = request
	inconsistent.Operation = InboxStatus
	if _, err := EncodeInboxRequest(inconsistent); err == nil {
		t.Fatal("status can carry an authority grant")
	}
	if ValidateInboxDisposition(InboxDispatch, InboxCustodyRecorded) == nil || ValidateInboxDisposition(InboxStatus, InboxDispatchArmed) == nil {
		t.Fatal("arm and custody results are interchangeable")
	}
}

func TestInboxFramesRejectAmbiguousOrMutatingStatus(t *testing.T) {
	request := inboxWireRequest()
	for _, mutate := range []func(*InboxRequest){
		func(r *InboxRequest) { r.Operation = InboxStatus },
		func(r *InboxRequest) { r.Target.ActorUID = "../other" },
		func(r *InboxRequest) { r.Target.ReceiverID = "" },
		func(r *InboxRequest) { r.Target.Digest = strings.Repeat("b", 64) },
		func(r *InboxRequest) { r.Payload = json.RawMessage(`[]`) },
	} {
		invalid := request
		mutate(&invalid)
		if _, err := EncodeInboxRequest(invalid); err == nil {
			t.Fatal("invalid submission encoded")
		}
	}
	status := InboxRequest{Operation: InboxStatus, Target: request.Target}
	frame, err := EncodeInboxRequest(status)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeInboxRequest(frame); err != nil || len(got.Payload) != 0 || got.Operation != InboxStatus {
		t.Fatalf("lookup is not read-only: %+v %v", got, err)
	}
	for _, bad := range [][]byte{
		append(append([]byte{}, frame...), []byte(`{}`)...),
		append(append([]byte{}, frame[:len(frame)-1]...), []byte(`,"operation":"admit"}`)...),
		append(append([]byte{}, frame[:len(frame)-1]...), []byte(`,"ok":true}`)...),
		bytes.Repeat([]byte{' '}, MaxMessageBytes+1),
	} {
		if _, err := DecodeInboxRequest(bad); err == nil {
			t.Fatal("ambiguous inbox frame decoded")
		}
	}
}
