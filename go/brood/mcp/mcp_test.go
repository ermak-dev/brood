package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/command"
)

type executorFunc func(context.Context, command.Invocation) (command.Result, error)

func (f executorFunc) Execute(ctx context.Context, i command.Invocation) (command.Result, error) {
	return f(ctx, i)
}

const handshake = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"

func responses(t *testing.T, b *bytes.Buffer) []map[string]any {
	t.Helper()
	var all []map[string]any
	d := json.NewDecoder(b)
	for {
		var r map[string]any
		err := d.Decode(&r)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	return all
}

// FEATURE: Catalog-backed tools-only MCP stdio adapter.
// SPEC: Lifecycle, schemas, argument validation and uncertain outcomes are shared.
// IMPLEMENTATION: Serve validates catalog invocations and preserves explicit outcomes.
// RESULT: IMPLEMENTED — real JSON-RPC streams cover discovery, calls and refusals.
// STATUS: DONE
func TestServeCatalogAndLifecycle(t *testing.T) {
	var out bytes.Buffer
	input := "{\"jsonrpc\":\"2.0\",\"id\":0,\"method\":\"tools/list\"}\n" + handshake + "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n"
	if err := Serve(context.Background(), strings.NewReader(input), &out, nil, command.Specs()); err != nil {
		t.Fatal(err)
	}
	all := responses(t, &out)
	if len(all) != 3 || all[0]["error"].(map[string]any)["code"] != float64(-32002) {
		t.Fatalf("responses: %v", all)
	}
	tools := all[2]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(command.Specs()) {
		t.Fatalf("tools=%d", len(tools))
	}
	for n, spec := range command.Specs() {
		tool := tools[n].(map[string]any)
		schema, _ := json.Marshal(spec.Schema())
		var want any
		_ = json.Unmarshal(schema, &want)
		if tool["name"] != spec.ToolName || !reflect.DeepEqual(tool["inputSchema"], want) {
			t.Fatalf("catalog drift: %v", tool)
		}
	}
}

func TestServeCallsAndUncertainty(t *testing.T) {
	for _, tc := range []struct {
		outcome string
		failed  bool
		err     error
	}{
		{"completed", false, nil}, {"accepted", false, nil}, {"unconfirmed", true, nil}, {"", true, errors.New("request refused")},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			input := handshake + "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"agents\",\"arguments\":{}}}\n"
			err := Serve(context.Background(), strings.NewReader(input), &out, executorFunc(func(ctx context.Context, i command.Invocation) (command.Result, error) {
				calls++
				if i.Name != "agents" {
					t.Fatalf("invocation: %+v", i)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("unbounded execution")
				}
				return command.Result{Text: "receipt", Outcome: tc.outcome}, tc.err
			}), command.Specs())
			if err != nil {
				t.Fatal(err)
			}
			all := responses(t, &out)
			result := all[1]["result"].(map[string]any)
			if calls != 1 || result["isError"] != tc.failed {
				t.Fatalf("calls=%d result=%v", calls, result)
			}
			if tc.err == nil && result["structuredContent"].(map[string]any)["outcome"] != tc.outcome {
				t.Fatalf("outcome lost: %v", result)
			}
		})
	}
}

func TestServeRejectsInvalidAndUnavailableTools(t *testing.T) {
	var out bytes.Buffer
	input := handshake + "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"send\",\"arguments\":{\"to\":\"peer\",\"message\":\"hi\",\"from\":\"spoof\"}}}\n" + "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"agents\",\"arguments\":{}}}\n"
	if err := Serve(context.Background(), strings.NewReader(input), &out, executorFunc(func(context.Context, command.Invocation) (command.Result, error) {
		t.Fatal("invalid invocation executed")
		return command.Result{}, nil
	}), nil); err != nil {
		t.Fatal(err)
	}
	all := responses(t, &out)
	if len(all) != 3 || all[1]["error"] == nil || all[2]["error"] == nil {
		t.Fatalf("responses: %v", all)
	}
}

// FEATURE: Guest discovery advertises start/reset with honest hints (#149).
// SPEC: Guest tools/list has 23 tools; start is neither read-only nor
// destructive, reset is destructive.
func TestGuestDiscoveryStartResetHints(t *testing.T) {
	var out bytes.Buffer
	input := handshake + "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n"
	if err := Serve(context.Background(), strings.NewReader(input), &out, nil, GuestSpecs()); err != nil {
		t.Fatal(err)
	}
	all := responses(t, &out)
	tools := all[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 23 {
		t.Fatalf("guest tools=%d", len(tools))
	}
	want := map[string]bool{"start": false, "reset": true}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		destructive, ok := want[tool["name"].(string)]
		if !ok {
			continue
		}
		hints := tool["annotations"].(map[string]any)
		if hints["readOnlyHint"] != false || hints["destructiveHint"] != destructive {
			t.Errorf("%s hints: %v", tool["name"], hints)
		}
		delete(want, tool["name"].(string))
	}
	if len(want) != 0 {
		t.Fatalf("guest discovery lacks %v", want)
	}
}

func TestTerminalWaitDeadline(t *testing.T) {
	if got := callTimeout(json.RawMessage(`{"wait":120}`)); got != 135*time.Second {
		t.Fatalf("terminal deadline=%v", got)
	}
	// Без ожидания send ждёт квитанцию доставки адресата (#260).
	for _, args := range []string{`{}`, `{"to":"peer","message":"hi"}`, `{"wait_seconds":301}`} {
		if got := callTimeout(json.RawMessage(args)); got != 30*time.Second {
			t.Errorf("%s: deadline=%v", args, got)
		}
	}
}

func TestServeFrameLimit(t *testing.T) {
	if err := Serve(context.Background(), strings.NewReader(strings.Repeat("x", maxFrameBytes+1)), io.Discard, nil, nil); err == nil {
		t.Fatal("oversized frame accepted")
	}
}
