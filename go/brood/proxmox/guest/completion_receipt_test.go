//go:build linux

package guest

// FEATURE: Клиент подтверждает разбор accepted до collector quiescence (#290).
// SPEC: Реальный Unix broker передаёт ACK только после точной client-квитанции;
// закрытие/ошибочный nonce/чужой request не становятся подтверждённым получением.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

func brokerReceipt() relay.CompletionReceipt {
	return relay.CompletionReceipt{Version: 1, SwarmID: "swarm", Pass: 1, Sequence: 2, RequestID: "delivery-2", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
}
func brokerCompletion() relay.SwarmRequest {
	return relay.SwarmRequest{Tool: "pipeline_visit_complete", Arguments: json.RawMessage(`{"run_id":"swarm","sequence":2,"request_id":"delivery-2","outcome":"success","report":"done"}`)}
}
func receiptBroker(t *testing.T, call relay.SwarmCaller) (*SwarmBroker, string) {
	t.Helper()
	if os.Geteuid() != 1000 {
		t.Skip("Unix peer fixture requires actor UID 1000")
	}
	path := filepath.Join(t.TempDir(), "swarm.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	broker := newSwarmBroker(context.Background(), listener, nil)
	release := broker.bind(context.Background(), call)
	t.Cleanup(func() { release(); broker.Close() })
	return broker, path
}

func TestCompletionClientAcknowledgesParsedAcceptanceThroughRootBroker(t *testing.T) {
	receipt := brokerReceipt()
	data, err := relay.WithCompletionReceipt(nil, receipt)
	if err != nil {
		t.Fatal(err)
	}
	ack := make(chan relay.SwarmRequest, 1)
	_, path := receiptBroker(t, func(_ context.Context, req relay.SwarmRequest) relay.SwarmResponse {
		if req.Tool == relay.CompletionReceiptAckTool {
			ack <- req
			return relay.SwarmResponse{OK: true, Text: "receipt recorded", Outcome: "confirmed"}
		}
		return relay.SwarmResponse{OK: true, Text: "completion accepted; observation pending", Outcome: "accepted", Data: data}
	})
	response := callSwarmBroker(context.Background(), path, brokerCompletion())
	if !response.OK || response.Outcome != "accepted" {
		t.Fatalf("accepted reply lost: %+v", response)
	}
	select {
	case request := <-ack:
		got, err := relay.DecodeCompletionReceiptAck(request)
		if err != nil || got != receipt {
			t.Fatal("root forwarded another completion receipt")
		}
	case <-time.After(time.Second):
		t.Fatal("parsed acceptance was not acknowledged to server")
	}
}

func TestCompletionBrokerNeverInfersClientReceiptFromResponseWrite(t *testing.T) {
	for _, kind := range []string{"closed", "wrong-nonce", "spoofed-supervisor-ack"} {
		t.Run(kind, func(t *testing.T) {
			var acks atomic.Int32
			data, err := relay.WithCompletionReceipt(nil, brokerReceipt())
			if err != nil {
				t.Fatal(err)
			}
			broker, path := receiptBroker(t, func(_ context.Context, req relay.SwarmRequest) relay.SwarmResponse {
				if req.Tool == relay.CompletionReceiptAckTool {
					acks.Add(1)
				}
				return relay.SwarmResponse{OK: true, Text: "accepted", Outcome: "accepted", Data: data}
			})
			if kind == "spoofed-supervisor-ack" {
				request, err := relay.CompletionReceiptAck(brokerReceipt())
				if err != nil {
					t.Fatal(err)
				}
				if response := callSwarmBroker(context.Background(), path, request); response.OK {
					t.Fatal("actor supplied a supervisor ACK")
				}
			} else {
				c, err := net.Dial("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				_ = c.SetDeadline(time.Now().Add(time.Second))
				raw, err := json.Marshal(brokerCompletion())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.Write(append(raw, '\n')); err != nil {
					t.Fatal(err)
				}
				response, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := relay.DecodeSwarmResponse(response); !ok {
					t.Fatal("invalid accepted response")
				}
				if kind == "wrong-nonce" {
					if _, err := c.Write([]byte("{\"nonce\":\"wrong\"}\n")); err != nil {
						t.Fatal(err)
					}
				}
				c.Close()
			}
			broker.Close()
			if acks.Load() != 0 {
				t.Fatal("root fabricated client receipt")
			}
		})
	}
}

func TestCompletionClientPreservesLegacyRepliesWithoutReceipt(t *testing.T) {
	for _, outcome := range []string{"completed", "accepted"} {
		t.Run(outcome, func(t *testing.T) {
			var calls atomic.Int32
			broker, path := receiptBroker(t, func(context.Context, relay.SwarmRequest) relay.SwarmResponse {
				calls.Add(1)
				return relay.SwarmResponse{OK: true, Text: "legacy reply", Outcome: outcome}
			})
			response := callSwarmBroker(context.Background(), path, brokerCompletion())
			broker.Close()
			if !response.OK || response.Outcome != outcome || response.Text != "legacy reply" || calls.Load() != 1 {
				t.Fatal("legacy completion invented an acknowledgement or changed its reply")
			}
		})
	}
}

func TestCompletionBrokerDoesNotAcknowledgeOtherRequestOrTool(t *testing.T) {
	for _, kind := range []string{"other-sequence", "other-attempt", "other-swarm", "other-tool"} {
		t.Run(kind, func(t *testing.T) {
			var acks atomic.Int32
			receipt := brokerReceipt()
			switch kind {
			case "other-sequence":
				receipt.Sequence++
			case "other-attempt":
				receipt.RequestID = "foreign"
			case "other-swarm":
				receipt.SwarmID = "foreign"
			}
			data, err := relay.WithCompletionReceipt(nil, receipt)
			if err != nil {
				t.Fatal(err)
			}
			broker, path := receiptBroker(t, func(_ context.Context, req relay.SwarmRequest) relay.SwarmResponse {
				if req.Tool == relay.CompletionReceiptAckTool {
					acks.Add(1)
				}
				return relay.SwarmResponse{OK: true, Text: "accepted", Outcome: "accepted", Data: data}
			})
			request := brokerCompletion()
			if kind == "other-tool" {
				request = relay.SwarmRequest{Tool: "agents"}
			}
			_ = callSwarmBroker(context.Background(), path, request)
			broker.Close()
			if acks.Load() != 0 {
				t.Fatal("unrelated request was acknowledged")
			}
		})
	}
}
