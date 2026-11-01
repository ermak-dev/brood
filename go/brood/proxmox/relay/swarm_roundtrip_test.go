package relay

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSwarmRoundTripAcrossTwoGuestConnections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := NewHub([]byte(strings.Repeat("k", 32)), func(uid string, generation int64, token string) bool {
		return (uid == "sender" || uid == "receiver") && generation == 1 && token == "synthetic-"+uid
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	server := httptest.NewTLSServer(h.Handler())
	defer server.Close()
	h.SetSwarmHandler(func(ctx context.Context, uid string, generation int64, request SwarmRequest) SwarmResponse {
		if uid != "sender" || generation != 1 || request.To != "receiver" {
			return SwarmResponse{Text: "refused"}
		}
		payload, _ := json.Marshal(MessageRequest{From: uid, To: "receiver", Body: request.Message})
		conn, err := h.DialMessage(ctx, "receiver", 1, payload)
		if err != nil {
			return SwarmResponse{Text: "recipient unavailable"}
		}
		defer conn.Close()
		result, err := ReadMessageResponse(conn)
		if err != nil {
			return SwarmResponse{Text: "unconfirmed"}
		}
		return SwarmResponse{OK: result.OK, Text: result.Verdict}
	})
	calls := make(chan SwarmCaller, 1)
	received := make(chan MessageRequest, 1)
	clients := make(chan error, 2)
	for _, uid := range []string{"sender", "receiver"} {
		cfg := ClientConfig{URL: "wss" + strings.TrimPrefix(server.URL, "https"), ActorUID: uid, Generation: 1, Token: "synthetic-" + uid, CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), HostKey: h.HostKey(), DialTarget: func(context.Context) (net.Conn, error) { return nil, errors.New("terminal unused") }}
		if uid == "sender" {
			cfg.OnSwarmSession = func(ctx context.Context, call SwarmCaller) { calls <- call; <-ctx.Done() }
		} else {
			cfg.HandleMessage = func(ctx context.Context, request MessageRequest) MessageResponse {
				received <- request
				return MessageResponse{OK: true, Verdict: "delivered"}
			}
		}
		go func() { clients <- RunClient(ctx, cfg) }()
	}
	defer func() {
		cancel()
		for range 2 {
			select {
			case <-clients:
			case <-time.After(5 * time.Second):
				t.Error("guest did not exit")
			}
		}
	}()
	await(t, func() bool { return h.Connected("sender", 1) && h.Connected("receiver", 1) })
	var call SwarmCaller
	select {
	case call = <-calls:
	case <-time.After(3 * time.Second):
		t.Fatal("no sender caller")
	}
	requestCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	result := call(requestCtx, SwarmRequest{Tool: "send", To: "receiver", Message: "synthetic swarm round trip"})
	if !result.OK || result.Text != "delivered" {
		t.Fatalf("result: %+v", result)
	}
	select {
	case message := <-received:
		if message.From != "sender" || message.Body != "synthetic swarm round trip" {
			t.Fatalf("message: %+v", message)
		}
	case <-requestCtx.Done():
		t.Fatal("no recipient message")
	}
}
