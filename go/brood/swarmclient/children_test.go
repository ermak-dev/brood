package swarmclient

// FEATURE: Guest child commands use the existing credential-free broker wire (#289).
// SPEC: CLI arguments remain canonical and contain no transport identity;
// unconfirmed replies preserve their journal data and produce an error.
// IMPLEMENTATION: New catalog invocations use the unchanged fixed-socket adapter.
// RESULT: IMPLEMENTED — canonical framing and uncertainty pass focused race checks.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"

	"brood.dev/go/brood/command"
)

func TestChildGuestWireCanonicalAndUnconfirmed(t *testing.T) {
	inv, err := command.ParseCLI([]string{"child", "create", "--parent-sequence", "1", "--parent-request-id", "parent", "--request-id", "one", "--role", "tests", "--task", "Run tests", "--total", "2"})
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer client.Close()
	requests := make(chan Request, 1)
	go func() {
		defer server.Close()
		var request Request
		_ = json.NewDecoder(server).Decode(&request)
		requests <- request
		_, _ = io.WriteString(server, "{\"ok\":false,\"text\":\"delivery may have occurred; not resent\",\"outcome\":\"unconfirmed\",\"data\":{\"child_id\":\"child\",\"delivery\":\"unconfirmed\"}}\n")
	}()
	result, err := exchange(context.Background(), client, inv)
	request := <-requests
	var args map[string]any
	if json.Unmarshal(request.Arguments, &args) != nil || request.Tool != "child_create" || request.To != "" || request.Message != "" || request.WaitSeconds != 0 || args["parent_request_id"] != "parent" || args["request_id"] != "one" || args["role"] != "tests" || args["mode"] != "all" {
		t.Fatalf("child wire: %+v %v", request, args)
	}
	if err == nil || result.Outcome != "unconfirmed" || string(result.Data) != `{"child_id":"child","delivery":"unconfirmed"}` {
		t.Fatalf("uncertainty or journal data lost: %+v %v", result, err)
	}
}
