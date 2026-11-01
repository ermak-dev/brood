//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func TestMCPToolsUseOnlySwarmBroker(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send","arguments":{"to":"peer","message":"hello","wait_seconds":2},"_meta":{"progressToken":"fixture-progress"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"agents","arguments":{}}}
`
	var out bytes.Buffer
	var calls []relay.SwarmRequest
	err := ServeMCP(context.Background(), strings.NewReader(input), &out, func(_ context.Context, req relay.SwarmRequest) relay.SwarmResponse {
		calls = append(calls, req)
		return relay.SwarmResponse{OK: true, Text: "submitted; delivery unconfirmed"}
	})
	if err != nil {
		t.Fatal(err)
	}
	var responses []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var response map[string]any
		if err := dec.Decode(&response); err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
	}
	if len(responses) != 4 || len(calls) != 2 {
		t.Fatalf("responses=%d calls=%v", len(responses), calls)
	}
	init := responses[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize: %v", init)
	}
	tools := responses[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools: %v", tools)
	}
	if calls[0].Tool != "send" || calls[0].To != "peer" || calls[0].WaitSeconds != 2 || calls[1].Tool != "agents" {
		t.Fatalf("calls: %v", calls)
	}
}

func TestMCPRejectsUninitializedUnknownAndSpoofedArguments(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}
{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"future"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send","arguments":{"to":"peer","message":"hello","from":"other-actor"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"exec","arguments":{}}}
`
	var out bytes.Buffer
	err := ServeMCP(context.Background(), strings.NewReader(input), &out, func(context.Context, relay.SwarmRequest) relay.SwarmResponse {
		t.Fatal("invalid tool reached broker")
		return relay.SwarmResponse{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), `"error"`) != 3 || !strings.Contains(out.String(), `"protocolVersion":"2025-03-26"`) {
		t.Fatalf("responses: %s", out.String())
	}
}
