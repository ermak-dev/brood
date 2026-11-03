//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/mcp"
	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: Brood orchestration through the shared command adapter.
// SPEC: Preserve agents/add/send and forward extended catalog arguments.
// IMPLEMENTATION: Share the MCP lifecycle and retain legacy scoped request mapping.
// RESULT: IMPLEMENTED — strict arguments, name bounds and server refusals verified.
// STATUS: DONE
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
	if len(tools) != len(mcp.GuestSpecs()) {
		t.Fatalf("tools: %v", tools)
	}
	var add map[string]any
	for i, spec := range mcp.GuestSpecs() {
		name := spec.ToolName
		tool := tools[i].(map[string]any)
		if name == "add" {
			add = tool
		}
		if tool["name"] != name {
			t.Fatalf("tool %d: %v", i, tool)
		}
		if tool["inputSchema"].(map[string]any)["additionalProperties"] != false {
			t.Fatalf("open schema: %v", tool)
		}
	}
	schema := add["inputSchema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	if len(properties) != 2 || properties["name"] == nil || properties["origin"] == nil || len(schema["required"].([]any)) != 1 || schema["required"].([]any)[0] != "name" || add["annotations"].(map[string]any)["readOnlyHint"] != false {
		t.Fatalf("add schema: %v", add)
	}
	if calls[0].Tool != "send" || calls[0].To != "peer" || calls[0].WaitSeconds != 2 || calls[1].Tool != "agents" {
		t.Fatalf("calls: %v", calls)
	}
}

func TestMCPAddContract(t *testing.T) {
	for _, tc := range []struct {
		args  string
		valid bool
	}{
		{`{"name":"peer-1"}`, true},
		{`{"name":"a"}`, true},
		{`{"name":"` + strings.Repeat("a", 63) + `"}`, true},
		{`{}`, false}, {`{"name":""}`, false},
		{`{"name":"Peer"}`, false}, {`{"name":"-peer"}`, false},
		{`{"name":"peer-"}`, false}, {`{"name":"peer_name"}`, false},
		{`{"name":"../peer"}`, false}, {`{"name":"two peers"}`, false},
		{`{"name":"` + strings.Repeat("a", 64) + `"}`, false},
		{`{"name":"peer","to":"other"}`, false},
		{`{"name":"peer","message":""}`, false},
		{`{"name":"peer","wait_seconds":0}`, false},
		{`{"name":"peer","from":"master"}`, false},
		{`{"name":"peer","actor_uid":"master"}`, false},
		{`{"name":"peer","generation":1}`, false},
		{`{"name":"peer","pool":"other"}`, false},
		{`{"name":"peer","token":"secret"}`, false},
		{`{"name":"peer","role":"master"}`, false},
	} {
		t.Run(tc.args, func(t *testing.T) {
			input := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" +
				`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add","arguments":` + tc.args + `}}` + "\n"
			var out bytes.Buffer
			calls := 0
			err := ServeMCP(context.Background(), strings.NewReader(input), &out, func(ctx context.Context, req relay.SwarmRequest) relay.SwarmResponse {
				calls++
				var args struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal([]byte(tc.args), &args)
				if !reflect.DeepEqual(req, relay.SwarmRequest{Tool: "add", To: args.Name}) {
					t.Fatalf("request: %+v", req)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("missing bounded deadline")
				}
				return relay.SwarmResponse{Text: "only an authorized master may create peers"}
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.valid && (calls != 1 || !strings.Contains(out.String(), `"isError":true`) || !strings.Contains(out.String(), "only an authorized master")) {
				t.Fatalf("calls=%d output=%s", calls, &out)
			}
			if !tc.valid && (calls != 0 || !strings.Contains(out.String(), `"code":-32602`)) {
				t.Fatalf("calls=%d output=%s", calls, &out)
			}
		})
	}
}

func TestMCPBroodInstructions(t *testing.T) {
	var out bytes.Buffer
	if err := ServeMCP(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n"), &out, nil); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			ServerInfo   struct{ Name string }
			Instructions string
		}
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.ServerInfo.Name != "brood" {
		t.Fatalf("serverInfo: %+v", response)
	}
	for _, text := range []string{"Brood", "mcp__brood__send", "mcp__brood__agents", "not upstream Mop", "agents()", "role", "only an authorized master", "send", "unconfirmed"} {
		if !strings.Contains(response.Result.Instructions, text) {
			t.Errorf("instructions missing %q: %s", text, response.Result.Instructions)
		}
	}
}

// FEATURE: Guest MCP exposes the shared catalog rather than a local tool list.
// SPEC: Every guest command is advertised in catalog order.
func TestGuestCatalogHasExtendedTools(t *testing.T) {
	invocation, err := command.ParseTool("session_history", json.RawMessage(`{"name":"peer"}`))
	if err != nil {
		t.Fatal(err)
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"session_history","arguments":{"name":"peer"}}}` + "\n"
	var out bytes.Buffer
	calls := 0
	err = ServeMCP(context.Background(), strings.NewReader(input), &out, func(_ context.Context, request relay.SwarmRequest) relay.SwarmResponse {
		calls++
		if request.Tool != "session_history" || string(request.Arguments) != string(invocation.Arguments) || request.To != "" || request.Message != "" || request.WaitSeconds != 0 {
			t.Fatalf("canonical request: %+v", request)
		}
		return relay.SwarmResponse{OK: true, Text: "fixture history"}
	})
	if err != nil || calls != 1 || !strings.Contains(out.String(), "fixture history") {
		t.Fatalf("calls=%d output=%s err=%v", calls, &out, err)
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
