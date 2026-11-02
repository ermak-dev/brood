//go:build linux

package sessionctl

// FEATURE: Identity-fenced terminal steps in the unprivileged helper.
// SPEC: Fixed argv, no shell or supplied paths, bounded snapshots, Escape interrupt,
// and an uncertain result after any potentially partial mutation.
// IMPLEMENTATION: Fixed tmux steps fenced by the existing live identity proof.
// RESULT: IMPLEMENTED — bounded UTF-8 screen snapshots and literal input argv.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTerminalSnapshotIsBoundedUTF8(t *testing.T) {
	e, _ := fixture(t)
	e.run = func(context.Context, ...string) (string, error) { return strings.Repeat("界", 6000), nil }
	raw, _ := json.Marshal(TerminalStep{Action: "read", Proof: terminalProof(identity{SessionID: "abc", Process: "boot:12", PID: 123})})
	r := call(e, "session_terminal", string(raw))
	if !r.OK {
		t.Fatal(r)
	}
	var result TerminalResult
	if err := json.Unmarshal([]byte(r.Text), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.Output) > 16*1024 || len(result.Output) < 16*1024-2 || strings.Contains(result.Output, "�") {
		t.Fatalf("snapshot length=%d truncated=%v", len(result.Output), result.Truncated)
	}
}

func TestTerminalStepFixedCommandsAndIdentity(t *testing.T) {
	e, _ := fixture(t)
	var commands [][]string
	e.run = func(_ context.Context, args ...string) (string, error) {
		commands = append(commands, args)
		return "screen\n", nil
	}
	invoke := func(s TerminalStep) TerminalResult {
		t.Helper()
		raw, _ := json.Marshal(s)
		r := call(e, "session_terminal", string(raw))
		if !r.OK {
			t.Fatal(r)
		}
		var out TerminalResult
		if json.Unmarshal([]byte(r.Text), &out) != nil {
			t.Fatal(r)
		}
		return out
	}
	opened := invoke(TerminalStep{Action: "open"})
	if opened.Proof == "" || len(commands) != 0 {
		t.Fatal(opened, commands)
	}
	got := invoke(TerminalStep{Action: "read", Proof: opened.Proof})
	if got.Output != "screen\n" {
		t.Fatal(got)
	}
	invoke(TerminalStep{Action: "input", Proof: opened.Proof, Data: "-x; $(id)\n"})
	invoke(TerminalStep{Action: "interrupt", Proof: opened.Proof})
	invoke(TerminalStep{Action: "resize", Proof: opened.Proof, Cols: 120, Rows: 40})
	expected := [][]string{{"capture-pane", "-p", "-t", target}, {"send-keys", "-t", target, "-l", "--", "-x; $(id)\n"}, {"send-keys", "-t", target, "Escape"}, {"resize-window", "-t", "claude:0", "-x", "120", "-y", "40"}}
	if !reflect.DeepEqual(commands, expected) {
		t.Fatalf("%q", commands)
	}
	e.prove = func(context.Context) (identity, error) { return identity{SessionID: "replacement"}, nil }
	before := len(commands)
	raw, _ := json.Marshal(TerminalStep{Action: "input", Proof: opened.Proof, Data: "bad"})
	if r := call(e, "session_terminal", string(raw)); r.OK || len(commands) != before {
		t.Fatal(r, commands)
	}
}
func TestTerminalStepUncertainAndBounds(t *testing.T) {
	e, _ := fixture(t)
	e.run = func(context.Context, ...string) (string, error) { return "", errors.New("partial") }
	proof := terminalProof(identity{SessionID: "abc", Process: "boot:12", PID: 123})
	for _, action := range []string{"input", "interrupt", "resize"} {
		raw, _ := json.Marshal(TerminalStep{Action: action, Proof: proof, Data: "hello", Cols: 80, Rows: 24})
		if r := call(e, "session_terminal", string(raw)); r.Outcome != "unknown" {
			t.Fatal(action, r)
		}
	}
	for _, raw := range []string{`{"action":"read","path":"/etc/passwd"}`, `{"action":"resize","cols":241,"rows":24}`, `{"action":"close"}`, `{"action":"input","data":"` + strings.Repeat("x", 4097) + `"}`} {
		if r := call(e, "session_terminal", raw); r.OK {
			t.Fatal(raw, r)
		}
	}
}
func TestTerminalStepRechecksAfterMutationAndRead(t *testing.T) {
	for _, action := range []string{"read", "input"} {
		e, _ := fixture(t)
		proof := terminalProof(identity{SessionID: "abc", Process: "boot:12", PID: 123})
		e.run = func(context.Context, ...string) (string, error) {
			e.prove = func(context.Context) (identity, error) { return identity{}, errors.New("gone") }
			return "secret", nil
		}
		raw, _ := json.Marshal(TerminalStep{Action: action, Proof: proof, Data: "hello"})
		r := call(e, "session_terminal", string(raw))
		if r.OK || strings.Contains(r.Text, "secret") || (action == "input" && r.Outcome != "unknown") {
			t.Fatal(action, r)
		}
	}
}
