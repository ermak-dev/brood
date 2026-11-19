//go:build linux

package guest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

func TestSupervisorInboxLostReceiptReadAfterAuthenticatedReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const token = "synthetic-inbox-relay-token"
	scope := deliverylog.Scope{ActorUID: "receiver", Incarnation: 2}
	hub, err := relay.NewHub([]byte(strings.Repeat("k", 32)), func(uid string, generation int64, candidate string) bool {
		return uid == scope.ActorUID && generation == scope.Incarnation && candidate == token
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(hub.Handler())
	t.Cleanup(func() { hub.Close(); server.Close() })
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	open := func() *deliverylog.Inbox {
		t.Helper()
		inbox, err := openScopedSupervisorInbox(ctx, base, scope)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = inbox.Close() })
		return inbox
	}
	first := open()
	payload := json.RawMessage(`{"to":"receiver","from":"operator","body":"exact fault case"}`)
	digest := sha256.Sum256(payload)
	target := relay.InboxTarget{ActorUID: scope.ActorUID, Incarnation: scope.Incarnation, ReceiverID: first.Identifier(), DeliveryID: "lost-reply", Digest: hex.EncodeToString(digest[:])}
	handler := SupervisorInboxEndpoint(first)
	committed := make(chan relay.InboxResponse, 1)
	start := func(handle func(context.Context, []byte) ([]byte, error)) func() {
		t.Helper()
		guestCtx, stop := context.WithCancel(ctx)
		ended := make(chan error, 1)
		cfg := relay.ClientConfig{URL: "wss" + strings.TrimPrefix(server.URL, "https"), ActorUID: scope.ActorUID,
			Generation: scope.Incarnation, Token: token, HostKey: hub.HostKey(),
			CAPEM:           pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
			DialTarget:      func(context.Context) (net.Conn, error) { return nil, errors.New("terminal not used") },
			InboxReceiverID: target.ReceiverID, HandleInbox: handle}
		go func() { ended <- relay.RunClient(guestCtx, cfg) }()
		shutdown := sync.OnceFunc(func() {
			stop()
			select {
			case <-ended:
			case <-time.After(5 * time.Second):
				t.Error("guest did not disconnect")
			}
		})
		t.Cleanup(shutdown)
		return shutdown
	}
	waitReceiver := func(want string) {
		t.Helper()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for hub.InboxReceiverID(scope.ActorUID, scope.Incarnation) != want {
			select {
			case <-ctx.Done():
				t.Fatalf("expected receiver state not reached: %q", want)
			case <-tick.C:
			}
		}
	}
	stop := start(func(ctx context.Context, frame []byte) ([]byte, error) {
		reply, err := handler(ctx, frame)
		if err != nil {
			return nil, err
		}
		receipt, err := relay.DecodeInboxResponse(reply, target)
		if err != nil {
			return nil, err
		}
		committed <- receipt
		return nil, errors.New("synthetic loss after durable commit")
	})
	waitReceiver(target.ReceiverID)
	if reply, err := hub.CallInbox(ctx, relay.InboxRequest{Operation: relay.InboxAdmit, Target: target, Payload: payload}); err == nil || reply != (relay.InboxResponse{}) {
		t.Fatalf("lost response appeared as confirmed: %+v %v", reply, err)
	}
	var original relay.InboxResponse
	select {
	case original = <-committed:
		if original.Disposition != relay.InboxCustodyRecorded {
			t.Fatalf("fault occurred before durable admission: %+v", original)
		}
	case <-ctx.Done():
		t.Fatal("receiving side did not commit")
	}
	stop()
	waitReceiver("")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := open()
	if reopened.Identifier() != target.ReceiverID {
		t.Fatal("reopened guest substituted another receiver identity")
	}
	start(SupervisorInboxEndpoint(reopened))
	waitReceiver(target.ReceiverID)
	if got, err := hub.CallInbox(ctx, relay.InboxRequest{Operation: relay.InboxStatus, Target: target}); err != nil || got != original {
		t.Fatalf("read-only recovery lost the retained receipt: %+v %v", got, err)
	}
	if entry, err := reopened.Lookup(ctx, target.ReceiverID, target.DeliveryID, target.Digest); err != nil || entry.State != deliverylog.InboxQueued {
		t.Fatalf("read-only recovery initiated a downstream session effect: %+v %v", entry, err)
	}
}

func TestSupervisorInboxDispatchArmsOnlyOneTrustedLaunch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	scope := deliverylog.Scope{ActorUID: "receiver", Incarnation: 1}
	inbox, err := openScopedSupervisorInbox(ctx, base, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	body := []byte(`{"body":"verified payload"}`)
	receipt, err := inbox.Enqueue(ctx, inbox.Identifier(), "step-1", body)
	if err != nil {
		t.Fatal(err)
	}
	target := relay.InboxTarget{ActorUID: scope.ActorUID, Incarnation: scope.Incarnation, ReceiverID: receipt.InboxID, DeliveryID: receipt.DeliveryID, Digest: receipt.Digest}
	handoff, err := NewInboxHandoff(ctx, inbox, "live-launch")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := SupervisorInboxWithDispatch(inbox, func(_ context.Context, launch string) *InboxHandoff {
		if launch == "live-launch" {
			return handoff
		}
		return nil
	})
	call := func(handler func(context.Context, []byte) ([]byte, error), launch string, want relay.InboxDisposition) {
		t.Helper()
		request := relay.InboxRequest{Operation: relay.InboxDispatch, Target: target, Grant: &relay.InboxDispatchGrant{LaunchID: launch, PermitID: "issued-once"}}
		frame, err := relay.EncodeInboxRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := handler(ctx, frame)
		if err != nil {
			t.Fatal(err)
		}
		observed, err := relay.DecodeInboxResponse(reply, target)
		if err != nil || observed.Disposition != want {
			t.Fatalf("wrong dispatch acknowledgement: %+v %v", observed, err)
		}
	}
	call(SupervisorInboxEndpoint(inbox), "live-launch", relay.InboxReceiptUnknown)
	call(endpoint, "stale-launch", relay.InboxReceiptUnknown)
	if _, err := handoff.Take(ctx, "live-launch"); err == nil {
		t.Fatal("not-current grant yielded payload")
	}
	call(endpoint, "live-launch", relay.InboxDispatchArmed)
	if work, err := handoff.Take(ctx, "live-launch"); err != nil || string(work.Payload) != string(body) {
		t.Fatalf("exact launch could not take exact bytes once: %v", err)
	}
	call(endpoint, "live-launch", relay.InboxReceiptUnknown)
	if _, err := handoff.Take(ctx, "live-launch"); err == nil {
		t.Fatal("resent grant rearmed work after claim")
	}
}

func TestSupervisorInboxEndpointRetainsOnlyTargetedCustody(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	scope := deliverylog.Scope{ActorUID: "guest-1", Incarnation: 7}
	inbox, err := openScopedSupervisorInbox(ctx, base, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	endpoint := SupervisorInboxEndpoint(inbox)
	payload := json.RawMessage(`{"to":"guest-1","from":"operator","body":"exact instruction"}`)
	hash := sha256.Sum256(payload)
	target := relay.InboxTarget{ActorUID: scope.ActorUID, Incarnation: scope.Incarnation, ReceiverID: inbox.Identifier(), DeliveryID: "delivery-1", Digest: hex.EncodeToString(hash[:])}

	call := func(request relay.InboxRequest, disposition relay.InboxDisposition) {
		t.Helper()
		frame, err := relay.EncodeInboxRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		response, err := endpoint(ctx, frame)
		if err != nil {
			t.Fatal(err)
		}
		got, err := relay.DecodeInboxResponse(response, request.Target)
		if err != nil || got.Disposition != disposition {
			t.Fatalf("unexpected receiving observation: %+v, %v (want %s)", got, err, disposition)
		}
	}
	status := relay.InboxRequest{Operation: relay.InboxStatus, Target: target}
	call(status, relay.InboxReceiptAbsent)
	foreign := target
	foreign.Incarnation++
	frame, err := relay.EncodeInboxRequest(relay.InboxRequest{Operation: relay.InboxAdmit, Target: foreign, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if reply, err := endpoint(ctx, frame); err == nil || reply != nil {
		t.Fatal("wrong incarnation obtained an admission response")
	}
	call(status, relay.InboxReceiptAbsent)
	call(relay.InboxRequest{Operation: relay.InboxAdmit, Target: target, Payload: payload}, relay.InboxCustodyRecorded)
	call(status, relay.InboxCustodyRecorded)
	wrong := status
	wrong.Target.Digest = hex.EncodeToString(make([]byte, 32))
	call(wrong, relay.InboxReceiptUnknown)
	// A lookup never claims the bytes or changes downstream session state.
	observed, err := inbox.Lookup(ctx, target.ReceiverID, target.DeliveryID, target.Digest)
	if err != nil || observed.State != deliverylog.InboxQueued {
		t.Fatalf("custody did not remain separate from downstream acceptance: %+v %v", observed, err)
	}
}
