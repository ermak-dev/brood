//go:build linux

package guest

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

func TestDurableMessageRetainedReceiptOverAuthenticatedRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const token = "synthetic-relay-test-token"
	hub, err := relay.NewHub([]byte(strings.Repeat("k", 32)), func(uid string, generation int64, candidate string) bool {
		return uid == "actor" && generation == 1 && candidate == token
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	server := httptest.NewTLSServer(hub.Handler())
	defer server.Close()
	journal := durableJournal(t, t.TempDir(), 2)
	var writes atomic.Int32
	handler := durableMessageHandler{journal: journal, supervisor: func() bool { return true }, transport: func(_ context.Context, req relay.MessageRequest) relay.MessageResponse {
		if req.DeliveryID != "" || req.JournalID != "" || req.Replay {
			return durableMessageUnknown("journal metadata crossed helper boundary")
		}
		writes.Add(1)
		return relay.MessageResponse{OK: true, Verdict: "delivered"}
	}}
	cfg := relay.ClientConfig{
		URL: strings.Replace(server.URL, "https://", "wss://", 1), ActorUID: "actor", Generation: 1, Token: token,
		HostKey: hub.HostKey(), CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
		DialTarget:           func(context.Context) (net.Conn, error) { return nil, errors.New("terminal unused") },
		MessageDeduplication: true, MessageJournalID: journal.Identifier(), HandleMessage: handler.receive,
	}
	done := make(chan error, 1)
	go func() { done <- relay.RunClient(ctx, cfg) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("relay client did not stop")
		}
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for hub.DeliveryJournalID("actor", 1) != journal.Identifier() {
		select {
		case <-ctx.Done():
			t.Fatal("authenticated journal never registered")
		case <-ticker.C:
		}
	}
	send := func(req relay.MessageRequest) relay.MessageResponse {
		t.Helper()
		frame, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := hub.DialMessage(ctx, "actor", 1, frame)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		response, err := relay.ReadMessageResponse(stream)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	req := relay.MessageRequest{DeliveryID: "server:instruction", JournalID: journal.Identifier(), To: "worker", From: "owner", Body: "work"}
	if first := send(req); !first.OK || first.Duplicate || first.NotSent {
		t.Fatalf("initial delivery: %+v", first)
	}
	req.Replay = true
	if duplicate := send(req); !duplicate.OK || !duplicate.Duplicate || duplicate.NotSent || writes.Load() != 1 {
		t.Fatalf("confirmed probe repeated helper: %+v writes=%d", duplicate, writes.Load())
	}
	req.DeliveryID = "missing-after-uncertain-write"
	if missing := send(req); missing.OK || missing.Duplicate || missing.NotSent || writes.Load() != 1 {
		t.Fatalf("missing probe became new delivery: %+v writes=%d", missing, writes.Load())
	}
}
