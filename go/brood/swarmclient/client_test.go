package swarmclient

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/command"
)

// FEATURE: Credential-free, bounded guest broker command transport.
// SPEC: Newline JSON framing; legacy and canonical envelopes never mix.
// IMPLEMENTATION: Fixed-socket client with bounded frames and caller deadlines.
// RESULT: IMPLEMENTED — net.Pipe verifies wire data and rejects partial responses.
// STATUS: DONE
func TestExchangeFramedRequest(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan map[string]any, 1)
	go func() {
		defer server.Close()
		var request map[string]any
		_ = json.NewDecoder(server).Decode(&request)
		done <- request
		_, _ = io.WriteString(server, "{\"ok\":true,\"text\":\"queued\",\"outcome\":\"accepted\"}\n")
	}()
	result, err := exchange(context.Background(), client, command.Invocation{Name: "send", Arguments: json.RawMessage(`{"to":"peer","message":"hello","wait_seconds":2}`)})
	request := <-done
	if err != nil || result.Outcome != "accepted" || request["tool"] != "send" || request["arguments"] != nil || request["to"] != "peer" {
		t.Fatalf("request=%v result=%+v err=%v", request, result, err)
	}
}

func TestCanonicalWaitDeadline(t *testing.T) {
	if got := requestTimeout(Request{Arguments: json.RawMessage(`{"wait_seconds":120}`)}); got != 135*time.Second {
		t.Fatalf("deadline=%v", got)
	}
	// Без ожидания send ждёт квитанцию доставки адресата (#260).
	for _, request := range []Request{{Tool: "send", Arguments: json.RawMessage(`{"to":"peer","message":"hi"}`)}, {Arguments: json.RawMessage(`{"wait_seconds":301}`)}} {
		if got := requestTimeout(request); got != 30*time.Second {
			t.Errorf("%+v: deadline=%v", request, got)
		}
	}
}

func TestExchangeRejectsOversizedAndPartialResponse(t *testing.T) {
	for _, response := range []string{strings.Repeat("x", maxFrameBytes+1), `{"ok":true,"text":"partial"}`} {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			_, _ = bufio.NewReader(server).ReadBytes('\n')
			_, _ = io.WriteString(server, response)
		}()
		_, err := exchange(context.Background(), client, command.Invocation{Name: "agents", Arguments: json.RawMessage(`{}`)})
		client.Close()
		if err == nil {
			t.Fatal("invalid response accepted")
		}
	}
}
