package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func capabilityMessage(t *testing.T, f *fixture, req MessageRequest) (MessageResponse, error) {
	t.Helper()
	data, err := encodeMessageFrame(req)
	if err != nil {
		return MessageResponse{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := f.h.DialMessage(ctx, "actor-uid", 3, data)
	if err != nil {
		return MessageResponse{}, err
	}
	defer stream.Close()
	return ReadMessageResponse(stream)
}

func TestMessageDedupRejectsDifferentJournalBeforeOpeningChannel(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.MessageDeduplication = true
	cfg.MessageJournalID = strings.Repeat("a", 64)
	var calls atomic.Int32
	cfg.HandleMessage = func(context.Context, MessageRequest) MessageResponse {
		calls.Add(1)
		return MessageResponse{OK: true, Verdict: "confirmed"}
	}
	start(t, f, cfg)
	response, err := capabilityMessage(t, f, MessageRequest{DeliveryID: "previous", JournalID: strings.Repeat("b", 64), Replay: true, To: "worker", From: "owner", Body: "work"})
	if !errors.Is(err, ErrDeliveryJournalChanged) || response.OK || calls.Load() != 0 {
		t.Fatalf("foreign journal reached session handler: response=%+v error=%v calls=%d", response, err, calls.Load())
	}
}

func TestMessageDedupCapabilityMustBeExplicitAndBoundToAuthenticatedConnection(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "dedup-v1"}[supported], func(t *testing.T) {
			f := setup(t)
			var calls atomic.Int32
			cfg := f.cfg
			cfg.MessageDeduplication = supported
			if supported {
				cfg.MessageJournalID = strings.Repeat("a", 64)
			}
			cfg.HandleMessage = func(_ context.Context, req MessageRequest) MessageResponse {
				calls.Add(1)
				return MessageResponse{OK: true, Duplicate: req.DeliveryID != "", Verdict: "confirmed"}
			}
			start(t, f, cfg)
			if got := f.h.SupportsMessageDeduplication("actor-uid", 3); got != supported {
				t.Fatalf("negotiated support=%v want %v", got, supported)
			}
			if f.h.SupportsMessageDeduplication("actor-uid", 2) || f.h.SupportsMessageDeduplication("foreign", 3) {
				t.Fatal("capability escaped authenticated incarnation")
			}
			req := MessageRequest{JournalID: strings.Repeat("a", 64), DeliveryID: "stable", To: "worker", From: "owner", Body: "work"}
			response, err := capabilityMessage(t, f, req)
			if supported {
				if err != nil || !response.OK || !response.Duplicate || calls.Load() != 1 {
					t.Fatalf("supported delivery metadata lost: %+v %v", response, err)
				}
			} else {
				if !errors.Is(err, ErrMessageDedupUnsupported) || calls.Load() != 0 {
					t.Fatalf("stable ID sent to legacy handler: calls=%d error=%v", calls.Load(), err)
				}
			}
			req.DeliveryID = ""
			req.JournalID = ""
			if response, err := capabilityMessage(t, f, req); err != nil || !response.OK {
				t.Fatalf("legacy send stopped working: %+v %v", response, err)
			}
			f.current.Store(false)
			if f.h.SupportsMessageDeduplication("actor-uid", 3) {
				t.Fatal("revoked guest retained capability")
			}
		})
	}
}

func TestMessageDedupCapabilityPinsJournalAcrossAuthenticatedReconnect(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.MessageDeduplication = true
	cfg.MessageJournalID = strings.Repeat("a", 64)
	var calls atomic.Int32
	cfg.HandleMessage = func(context.Context, MessageRequest) MessageResponse {
		calls.Add(1)
		return MessageResponse{OK: true, Verdict: "confirmed"}
	}
	stopOld := start(t, f, cfg)
	old := f.h.session("actor-uid", 3)
	if got := f.h.DeliveryJournalID("actor-uid", 3); got != cfg.MessageJournalID {
		t.Fatal("authenticated journal identity not retained")
	}
	cfg.MessageJournalID = strings.Repeat("b", 64)
	start(t, f, cfg)
	await(t, func() bool { return f.h.session("actor-uid", 3) != old })
	stopOld()
	if got := f.h.DeliveryJournalID("actor-uid", 3); got != cfg.MessageJournalID {
		t.Fatal("journal identity was sticky across reconnect")
	}
	_, err := capabilityMessage(t, f, MessageRequest{DeliveryID: "uncertain", JournalID: strings.Repeat("a", 64), Replay: true, To: "worker", From: "owner", Body: "work"})
	if !errors.Is(err, ErrDeliveryJournalChanged) || calls.Load() != 0 {
		t.Fatalf("replacement journal received uncertain old instruction: %v calls=%d", err, calls.Load())
	}
}

func TestMessageDedupCapabilityDoesNotSurviveLegacyReconnect(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.MessageDeduplication = true
	cfg.MessageJournalID = strings.Repeat("a", 64)
	cfg.HandleMessage = func(context.Context, MessageRequest) MessageResponse {
		return MessageResponse{OK: true, Verdict: "confirmed"}
	}
	stopOld := start(t, f, cfg)
	if !f.h.SupportsMessageDeduplication("actor-uid", 3) {
		t.Fatal("new guest did not advertise support")
	}
	old := f.h.session("actor-uid", 3)
	var legacyCalls atomic.Int32
	legacy := f.cfg
	legacy.HandleMessage = func(context.Context, MessageRequest) MessageResponse {
		legacyCalls.Add(1)
		return MessageResponse{OK: true, Verdict: "legacy"}
	}
	start(t, f, legacy)
	await(t, func() bool { return f.h.session("actor-uid", 3) != old })
	stopOld()
	if f.h.SupportsMessageDeduplication("actor-uid", 3) {
		t.Fatal("capability was sticky across connection replacement")
	}
	_, err := capabilityMessage(t, f, MessageRequest{JournalID: strings.Repeat("a", 64), DeliveryID: "stable", To: "worker", From: "owner", Body: "work"})
	if !errors.Is(err, ErrMessageDedupUnsupported) || legacyCalls.Load() != 0 {
		t.Fatalf("cached prior capability authorized legacy write: %v calls=%d", err, legacyCalls.Load())
	}
}

func TestMessageDedupAdvertisementRequiresExactlyOneCanonicalJournal(t *testing.T) {
	for _, journals := range [][]string{nil, {""}, {"short"}, {strings.Repeat("A", 64)}, {strings.Repeat("a", 64), strings.Repeat("a", 64)}} {
		header := http.Header{}
		header.Set(messageProtocolHeader, messageDedupProtocol)
		for _, id := range journals {
			header.Add(messageJournalHeader, id)
		}
		if _, err := messageDedupAdvertisement(header); err == nil {
			t.Errorf("invalid journal advertisement accepted: %q", journals)
		}
	}
	header := http.Header{}
	header.Set(messageJournalHeader, strings.Repeat("a", 64))
	if _, err := messageDedupAdvertisement(header); err == nil {
		t.Fatal("orphan journal header accepted")
	}
	header.Set(messageProtocolHeader, "dedup-v1")
	if _, err := messageDedupAdvertisement(header); err == nil {
		t.Fatal("old protocol advertised safe journal binding")
	}
	header.Set(messageProtocolHeader, messageDedupProtocol)
	if id, err := messageDedupAdvertisement(header); err != nil || id != strings.Repeat("a", 64) {
		t.Fatal("canonical journal identity lost")
	}
}

func TestMessageDedupAdvertisementRejectsAmbiguousAndUnknownVersionsAfterAuth(t *testing.T) {
	for _, values := range [][]string{{""}, {"dedup-v2"}, {"dedup-v1", "dedup-v1"}, {"dedup-v1, dedup-v2"}} {
		f := setup(t)
		req, err := http.NewRequest("GET", f.server.URL+"/api/proxmox/guest/actor-uid/3", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		for _, value := range values {
			req.Header.Add("X-Brood-Message-Protocol", value)
		}
		response, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "invalid message delivery protocol advertisement") {
			t.Fatalf("ambiguous capability accepted or hidden by websocket failure: status=%d body=%s", response.StatusCode, body)
		}
		if f.h.SupportsMessageDeduplication("actor-uid", 3) {
			t.Fatal("HTTP header alone registered a capability")
		}
	}
	f := setup(t)
	req, _ := http.NewRequest("GET", f.server.URL+"/api/proxmox/guest/actor-uid/3", nil)
	req.Header.Set("X-Brood-Message-Protocol", "dedup-v2")
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("capability parsing bypassed authentication")
	}
}

func TestMessageDedupDeliveryCancelledDuringAuthorityCheckNeverOpensChannel(t *testing.T) {
	f := setup(t)
	previous := f.h.authorize
	var cancelDelivery atomic.Pointer[context.CancelFunc]
	f.h.authorize = func(uid string, generation int64, token string) bool {
		if cancel := cancelDelivery.Load(); cancel != nil {
			(*cancel)()
		}
		return previous(uid, generation, token)
	}
	cfg := f.cfg
	cfg.MessageDeduplication = true
	cfg.MessageJournalID = strings.Repeat("a", 64)
	var calls atomic.Int32
	cfg.HandleMessage = func(context.Context, MessageRequest) MessageResponse {
		calls.Add(1)
		return MessageResponse{OK: true, Verdict: "confirmed"}
	}
	start(t, f, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelDelivery.Store(&cancel)
	frame, err := encodeMessageFrame(MessageRequest{JournalID: strings.Repeat("a", 64), DeliveryID: "cancelled", To: "worker", From: "owner", Body: "work"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := f.h.DialMessage(ctx, "actor-uid", 3, frame)
	if stream != nil {
		stream.Close()
	}
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("cancelled authority check sent a frame: error=%v handler calls=%d", err, calls.Load())
	}
}

func TestMessageDedupHeaderCannotRegisterBeforeSSHAuthentication(t *testing.T) {
	for _, failure := range []string{"password", "identity"} {
		t.Run(failure, func(t *testing.T) {
			f := setup(t)
			cfg := f.cfg
			cfg.MessageDeduplication = true
			cfg.MessageJournalID = strings.Repeat("a", 64)
			cfg.HandleMessage = func(context.Context, MessageRequest) MessageResponse {
				return MessageResponse{OK: true, Verdict: "confirmed"}
			}
			opts, err := options(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "password" {
				opts.ssh.Auth = []ssh.AuthMethod{ssh.Password("incorrect")}
			} else {
				opts.ssh.User = "another-actor/3"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if runAttempt(ctx, opts) {
				t.Fatal("failed SSH identity reported connected")
			}
			if f.h.SupportsMessageDeduplication("actor-uid", 3) || f.h.Connected("actor-uid", 3) {
				t.Fatal("HTTP capability header registered an unauthenticated SSH peer")
			}
		})
	}
}

func TestClientMessageDedupCapabilityRequiresHandlerAndKeepsDefaultAbsent(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.MessageDeduplication = true
	cfg.MessageJournalID = strings.Repeat("a", 64)
	if _, err := options(cfg); err == nil {
		t.Fatal("client advertised dedup without message handler")
	}
	cfg.HandleMessage = func(context.Context, MessageRequest) MessageResponse {
		return MessageResponse{OK: true, Verdict: "confirmed"}
	}
	cfg.MessageJournalID = ""
	if _, err := options(cfg); err == nil {
		t.Fatal("durable handler without journal identity advertised capability")
	}
	cfg.MessageJournalID = strings.Repeat("a", 64)
	configured, err := options(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if configured.ws.Header.Get("X-Brood-Message-Protocol") != "dedup-v2" || configured.ws.Header.Get(messageJournalHeader) != cfg.MessageJournalID {
		t.Fatal("client capability not carried on authenticated connection")
	}
	cfg.MessageDeduplication = false
	cfg.MessageJournalID = ""
	legacy, err := options(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := legacy.ws.Header["X-Brood-Message-Protocol"]; exists {
		t.Fatal("legacy client silently advertised a protocol")
	}
}
