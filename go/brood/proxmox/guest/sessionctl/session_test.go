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
	"io"
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
func TestProcessOutputCapCancelsAtWriteBoundary(t *testing.T) {
	canceled := false
	out := &cappedOutput{cancel: func() { canceled = true }}
	if _, err := out.Write([]byte(strings.Repeat("x", 48*1024+1))); err == nil || !canceled || out.Len() != 0 {
		t.Fatal("output was buffered past cap")
	}
}

// os/exec copies the pipe into Stdout with io.Copy, which prefers a ReadFrom
// of the writer over its Write (#225).
func TestProcessOutputCapHoldsUnderCopy(t *testing.T) {
	canceled := false
	out := &cappedOutput{cancel: func() { canceled = true }}
	pipe := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 48*1024+1))}
	if _, err := io.Copy(out, pipe); err == nil || !canceled || out.Len() > 48*1024 {
		t.Fatalf("output was copied past cap: canceled=%v len=%d", canceled, out.Len())
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
