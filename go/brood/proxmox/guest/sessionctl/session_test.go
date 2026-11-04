//go:build linux

package sessionctl

// FEATURE: Safe local session operations, never arbitrary commands or paths.
// SPEC: Fixed PTY holder and defaults; bounded, identity-bound transcript pages;
// incomplete JSON stays pending, symlinks fail, usage deduplicates message IDs.
// IMPLEMENTATION: Bounded tail/history/stat, exact slash allowlist and live fences.
// RESULT: IMPLEMENTED — bounded session reads and slash; destructive operations refuse.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
	"brood.dev/go/brood/proxmox/relay"
)

func fixture(t *testing.T) (*engine, string) {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, "projects", "-workspace-project")
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	e := &engine{config: root, now: func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }, prove: func(context.Context) (identity, error) {
		return fixtureIdentity(), nil
	}}
	return e, filepath.Join(p, "abc.jsonl")
}
func call(e *engine, op, args string) relay.SessionResponse {
	return e.handle(context.Background(), relay.SessionRequest{Operation: op, Arguments: json.RawMessage(args), Caller: "user:42"})
}
func TestHistoryPendingCursorAndReplacement(t *testing.T) {
	e, p := fixture(t)
	os.WriteFile(p, []byte("{\"type\":\"user\",\"message\":{\"content\":\"hello\"}}\n{\"type\":"), 0600)
	first := call(e, "session_history", `{"name":"routing-only"}`)
	if !first.OK {
		t.Fatal(first)
	}
	var page historyPage
	if err := json.Unmarshal([]byte(first.Text), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || !page.Partial || page.Cursor == "" || page.SessionID != "abc" {
		t.Fatalf("%#v", page)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("\"assistant\"}\n")
	f.Close()
	args, _ := json.Marshal(map[string]any{"name": "ignored", "cursor": page.Cursor})
	next := call(e, "session_history", string(args))
	if !next.OK {
		t.Fatal(next)
	}
	var second historyPage
	json.Unmarshal([]byte(next.Text), &second)
	if second.Reset || len(second.Records) != 1 || second.Partial {
		t.Fatalf("%#v", second)
	}
	e.prove = func(context.Context) (identity, error) {
		return identity{SessionID: "abc", Process: "boot:13", PID: 123}, nil
	}
	replaced := call(e, "session_history", string(args))
	json.Unmarshal([]byte(replaced.Text), &second)
	if !replaced.OK || !second.Reset || len(second.Records) != 2 {
		t.Fatalf("%#v %#v", replaced, second)
	}
}
func TestHistoryRewriteStartsNewEpoch(t *testing.T) {
	e, p := fixture(t)
	os.WriteFile(p, []byte(`{"text":"before"}`+"\n"), 0600)
	got := call(e, "session_history", `{}`)
	var first historyPage
	json.Unmarshal([]byte(got.Text), &first)
	os.WriteFile(p, []byte(`{"text":"after!"}`+"\n"), 0600)
	args, _ := json.Marshal(map[string]any{"cursor": first.Cursor})
	got = call(e, "session_history", string(args))
	var second historyPage
	json.Unmarshal([]byte(got.Text), &second)
	if !got.OK || !second.Reset || second.Epoch == first.Epoch || len(second.Records) != 1 {
		t.Fatalf("%#v %#v %#v", got, first, second)
	}
}
func TestHistoryRefusesSymlinkAndBadCursor(t *testing.T) {
	e, p := fixture(t)
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("{}\n"), 0600)
	os.Symlink(secret, p)
	if got := call(e, "session_history", `{}`); got.OK {
		t.Fatal("symlink read", got)
	}
	os.Remove(p)
	os.WriteFile(p, []byte("{}\n"), 0600)
	if got := call(e, "session_history", `{"cursor":"garbage"}`); got.OK {
		t.Fatal("bad cursor accepted")
	}
	if got := call(e, "session_history", `{"path":"/etc/passwd"}`); got.OK {
		t.Fatal("arbitrary path accepted")
	}
}
func TestTailAndSlashAreFixedAndValidated(t *testing.T) {
	e, _ := fixture(t)
	var calls []ptysession.Request
	e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
		calls = append(calls, req)
		return ptysession.Reply{OK: true, Screen: "one\nneedle\nthree\n"}, nil
	}
	got := call(e, "tail", `{"name":";ignored","grep":"needle"}`)
	if !got.OK || got.Text != "needle\n" || calls[0].Action != "tail" || calls[0].Lines != 40 || calls[0].Proof != fixtureIdentity().HolderProof {
		t.Fatalf("%#v %v", got, calls)
	}
	for _, args := range []string{`{"command":"/status\n/clear"}`, `{"command":"/evil"}`, `{"command":"/model;id"}`, `{"command":"Escape\u001b"}`} {
		if got := call(e, "slash", args); got.OK {
			t.Fatal("unsafe slash accepted", args)
		}
	}
	calls = nil
	if got := call(e, "slash", `{"command":"/status"}`); !got.OK || got.Outcome != "submitted" {
		t.Fatal(got)
	}
	if len(calls) != 1 || calls[0].Action != "input" || calls[0].Proof != fixtureIdentity().HolderProof || string(calls[0].Data) != "/status\r" {
		t.Fatal(calls)
	}
}
func TestTailReturnsRequestedLinesNotScrollbackPlusScreen(t *testing.T) {
	e, _ := fixture(t)
	e.call = func(context.Context, ptysession.Request) (ptysession.Reply, error) {
		return ptysession.Reply{OK: true, Screen: "old\nolder\nline1\nline2\nline3\n"}, nil
	}
	got := call(e, "tail", `{"lines":3}`)
	if !got.OK || got.Text != "line1\nline2\nline3\n" {
		t.Fatal(got)
	}
}
func TestStatDeduplicatesAndReportsUnknown(t *testing.T) {
	e, p := fixture(t)
	row := `{"type":"assistant","timestamp":"2026-11-02T01:00:00Z","message":{"id":"msg1","usage":{"input_tokens":12,"output_tokens":7}}}` + "\n"
	os.WriteFile(p, []byte(row+row+"{bad}\n"), 0600)
	got := call(e, "stat", `{}`)
	if !got.OK {
		t.Fatal(got)
	}
	var report usageReport
	json.Unmarshal([]byte(got.Text), &report)
	if report.Input != 12 || report.Output != 7 || report.Attribution != "unknown" || !report.Partial {
		t.Fatalf("%#v", report)
	}
}
func TestStatPendingRecordAndSubagentDedup(t *testing.T) {
	e, p := fixture(t)
	row := `{"type":"assistant","timestamp":"2026-11-02T01:00:00Z","message":{"id":"same","usage":{"input_tokens":12}}}`
	os.WriteFile(p, []byte(row+"\n"), 0600)
	sub := filepath.Join(filepath.Dir(p), "abc", "subagents")
	os.MkdirAll(sub, 0700)
	os.WriteFile(filepath.Join(sub, "agent.jsonl"), []byte(row+"\n"), 0600)
	pending := strings.ReplaceAll(row, "same", "pending")
	os.WriteFile(filepath.Join(sub, "pending.jsonl"), []byte(pending), 0600)
	got := call(e, "stat", `{}`)
	var report usageReport
	json.Unmarshal([]byte(got.Text), &report)
	if !got.OK || report.Input != 12 || !report.Partial {
		t.Fatalf("%#v %#v", got, report)
	}
}
func TestProcessOutputCapCancelsAtWriteBoundary(t *testing.T) {
	canceled := false
	out := &cappedOutput{cancel: func() { canceled = true }}
	if _, err := out.Write([]byte(strings.Repeat("x", 48*1024+1))); err == nil || !canceled || out.Len() != 0 {
		t.Fatal("output was buffered past cap")
	}
}
func TestHistoryRefusesChangedIdentityDuringRead(t *testing.T) {
	e, p := fixture(t)
	os.WriteFile(p, []byte(`{"private":"record"}`+"\n"), 0600)
	calls := 0
	e.prove = func(context.Context) (identity, error) {
		calls++
		return identity{SessionID: "abc", Process: fmt.Sprint(calls)}, nil
	}
	got := call(e, "session_history", `{}`)
	if got.OK || strings.Contains(got.Text, "private") {
		t.Fatal(got)
	}
}
func TestUnsupportedOperationsFailExplicitly(t *testing.T) {
	e, _ := fixture(t)
	for _, op := range []string{"wipe", "recycle", "workspace"} {
		got := call(e, op, `{}`)
		if got.OK || got.Outcome != "unsupported" || got.Text == "" {
			t.Fatalf("%s: %#v", op, got)
		}
	}
	for _, args := range []string{`{"lines":0}`, `{"lines":10001}`, `{"lines":null}`, `{"grep":"\u001b"}`, `null`} {
		if got := call(e, "tail", args); got.OK {
			t.Fatal(args)
		}
	}
}
