package command_test

// FEATURE: Shared guest child-task command declarations (#289).
// SPEC: CLI and MCP share canonical bounded arguments; task IDs are not graph
// step identity, request payloads never choose principal/project/URL, and join
// waits are bounded to 15 seconds with retained obligations.
// IMPLEMENTATION: Shared five-command declarations and strict canonical parsing.
// RESULT: IMPLEMENTED — full Go suite and focused race checks pass.
// STATUS: DONE

import (
	"encoding/json"
	"reflect"
	"testing"

	"brood.dev/go/brood/command"
)

func TestChildCommandCatalogAndParity(t *testing.T) {
	for _, name := range []string{"child_create", "child_get", "child_join", "child_cancel", "child_complete"} {
		s, ok := command.Lookup(name)
		if !ok || !s.Guest || s.ReadOnly != (name == "child_get") {
			t.Fatalf("child metadata %s: %+v", name, s)
		}
	}
	raw := json.RawMessage(`{"parent_sequence":1,"parent_request_id":"parent","request_id":"task-1","role":"test","task":"Run tests","total":2}`)
	got, err := command.ParseTool("child_create", raw)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := command.ParseCLI([]string{"child", "create", "--parent-sequence", "1", "--parent-request-id", "parent", "--request-id", "task-1", "--role", "test", "--task", "Run tests", "--total", "2"})
	if err != nil || !reflect.DeepEqual(got, cli) {
		t.Fatalf("CLI/MCP parity %+v %+v %v", got, cli, err)
	}
	var args map[string]any
	if err := json.Unmarshal(got.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["mode"] != "all" || args["quorum"] != float64(0) {
		t.Fatalf("policy defaults %v", args)
	}
}

func TestChildCommandBoundariesAndIdentity(t *testing.T) {
	valid := `{"parent_sequence":1,"parent_request_id":"p","request_id":"c","role":"test","task":"Run tests","total":1`
	for _, suffix := range []string{`,"actor_uid":"other"}`, `,"project_id":"other"}`, `,"url":"https://invalid.example"}`, `,"role":"other"}`, `,"request_id":"duplicate"}`} {
		if _, err := command.ParseTool("child_create", json.RawMessage(valid+suffix)); err == nil {
			t.Fatalf("accepted identity/bound violation: %s", suffix)
		}
	}
	for _, tc := range []struct {
		field string
		value any
	}{
		{"total", 65}, {"total", 0}, {"total", 1.5}, {"total", nil},
		{"mode", "cancel_remaining"}, {"task", nil}, {"role", nil},
	} {
		var args map[string]any
		if err := json.Unmarshal([]byte(valid+`}`), &args); err != nil {
			t.Fatal(err)
		}
		args[tc.field] = tc.value
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := command.ParseTool("child_create", raw); err == nil {
			t.Fatalf("accepted %s=%v without a duplicate-key distraction", tc.field, tc.value)
		}
	}
	for _, wait := range []string{"-1", "16", "300", "null"} {
		if _, err := command.ParseTool("child_join", json.RawMessage(`{"parent_sequence":1,"parent_request_id":"p","wait_seconds":`+wait+`}`)); err == nil {
			t.Fatalf("accepted join wait %s", wait)
		}
	}
	for _, wait := range []string{"0", "15"} {
		if _, err := command.ParseTool("child_join", json.RawMessage(`{"parent_sequence":1,"parent_request_id":"p","wait_seconds":`+wait+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := command.ParseTool("child_complete", json.RawMessage(`{"child_id":"child","request_id":"c","outcome":"success","sequence":1}`)); err == nil {
		t.Fatal("child completion accepted graph sequence")
	}
}
