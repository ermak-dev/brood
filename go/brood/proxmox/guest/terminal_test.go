//go:build linux

package guest

// FEATURE: Private bounded session terminal attachments owned by a connection.
// SPEC: Caller-bound random IDs, one writer per proven session, consecutive
// sequence replay cache, snapshot cursors, expiry and no tmux kill on close.
// IMPLEMENTATION: Connection-owned metadata, fixed helper steps and cached outcomes.
// RESULT: IMPLEMENTED — private snapshot attachments with fail-closed writers.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

type terminalFixture struct {
	c       *TerminalController
	now     time.Time
	steps   []sessionctl.TerminalStep
	proof   string
	unknown bool
	output  string
}

func terminalFixtureNew(t *testing.T) *terminalFixture {
	t.Helper()
	f := &terminalFixture{now: time.Now(), proof: "session-a", output: "screen"}
	f.c = NewTerminalController(func(_ context.Context, s sessionctl.TerminalStep) relay.SessionResponse {
		f.steps = append(f.steps, s)
		if s.Proof != "" && s.Proof != f.proof {
			return relay.SessionResponse{Outcome: "failed", Text: "identity changed"}
		}
		if f.unknown && s.Action == "input" {
			return relay.SessionResponse{Outcome: "unknown", Text: "uncertain"}
		}
		b, _ := json.Marshal(sessionctl.TerminalResult{Proof: f.proof, Output: f.output})
		return relay.SessionResponse{OK: true, Outcome: "complete", Text: string(b)}
	})
	f.c.now = func() time.Time { return f.now }
	return f
}
func terminalCall(c *TerminalController, caller, args string) relay.SessionResponse {
	return c.Handle(context.Background(), relay.SessionRequest{Operation: "session_terminal", Caller: caller, Arguments: json.RawMessage(args)})
}
func terminalPage(t *testing.T, r relay.SessionResponse) TerminalResponse {
	t.Helper()
	if !r.OK {
		t.Fatal(r)
	}
	var p TerminalResponse
	if json.Unmarshal([]byte(r.Text), &p) != nil {
		t.Fatal(r)
	}
	return p
}
func TestTerminalConcurrentDuplicateInputRunsOnce(t *testing.T) {
	f := terminalFixtureNew(t)
	a := terminalPage(t, terminalCall(f.c, "a", `{"action":"open","mode":"rw"}`))
	request := fmt.Sprintf(`{"action":"input","tid":%q,"seq":1,"data":"one delivery"}`, a.TID)
	before := len(f.steps)
	var wg sync.WaitGroup
	responses := make(chan relay.SessionResponse, 16)
	for i := 0; i < 16; i++ {
		wg.Go(func() { responses <- terminalCall(f.c, "a", request) })
	}
	wg.Wait()
	close(responses)
	var first relay.SessionResponse
	for r := range responses {
		if !r.OK {
			t.Fatal(r)
		}
		if first.Outcome == "" {
			first = r
		}
		if first != r {
			t.Fatal("duplicate response changed", r)
		}
	}
	if len(f.steps) != before+1 {
		t.Fatal("duplicate input replayed", f.steps)
	}
}

func TestTerminalSessionHandlerDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler := NewSessionHandler(ctx)
	r := handler(context.Background(), relay.SessionRequest{Operation: "session_terminal", Caller: "a", Arguments: json.RawMessage(`{"action":"open"}`)})
	if r.OK || !strings.Contains(r.Text, "closed") {
		t.Fatal(r)
	}
}
func TestTerminalPrivateSequenceAndClose(t *testing.T) {
	f := terminalFixtureNew(t)
	a := terminalPage(t, terminalCall(f.c, "alice", `{"action":"open","mode":"rw"}`))
	if a.TID == "" || a.Mode != "rw" || a.Semantics != "screen-snapshot" {
		t.Fatal(a)
	}
	if terminalCall(f.c, "bob", `{"action":"open","mode":"rw"}`).OK {
		t.Fatal("two writers")
	}
	req := fmt.Sprintf(`{"action":"input","tid":%q,"seq":1,"data":"hello"}`, a.TID)
	before := len(f.steps)
	if terminalCall(f.c, "bob", req).OK {
		t.Fatal("wrong caller")
	}
	first := terminalCall(f.c, "alice", req)
	if !first.OK {
		t.Fatal(first)
	}
	if again := terminalCall(f.c, "alice", req); again != first || len(f.steps) != before+1 {
		t.Fatal("input replayed", again, f.steps)
	}
	if terminalCall(f.c, "alice", strings.Replace(req, "hello", "changed", 1)).OK {
		t.Fatal("changed replay")
	}
	if terminalCall(f.c, "alice", fmt.Sprintf(`{"action":"interrupt","tid":%q,"seq":3}`, a.TID)).OK {
		t.Fatal("sequence gap")
	}
	before = len(f.steps)
	closeReq := fmt.Sprintf(`{"action":"close","tid":%q,"seq":2}`, a.TID)
	closed := terminalCall(f.c, "alice", closeReq)
	if !closed.OK || len(f.steps) != before {
		t.Fatal(closed, f.steps)
	}
	if terminalCall(f.c, "alice", closeReq) != closed {
		t.Fatal("close replay")
	}
	terminalPage(t, terminalCall(f.c, "bob", `{"action":"open","mode":"rw"}`))
}
func TestTerminalReadOnlySnapshotAndExpiry(t *testing.T) {
	f := terminalFixtureNew(t)
	a := terminalPage(t, terminalCall(f.c, "alice", `{"action":"open"}`))
	if a.Mode != "ro" {
		t.Fatal(a)
	}
	if terminalCall(f.c, "alice", fmt.Sprintf(`{"action":"input","tid":%q,"seq":1,"data":"no"}`, a.TID)).OK {
		t.Fatal("ro input")
	}
	read := func(seq int, cursor string) TerminalResponse {
		return terminalPage(t, terminalCall(f.c, "alice", fmt.Sprintf(`{"action":"read","tid":%q,"seq":%d,"cursor":%q}`, a.TID, seq, cursor)))
	}
	first := read(1, "")
	if !first.Reset || first.Output != "screen" || first.Cursor == "" {
		t.Fatal(first)
	}
	second := read(2, first.Cursor)
	if second.Reset || second.Output != "" || second.Cursor != first.Cursor {
		t.Fatal(second)
	}
	f.output = "replacement screen"
	third := read(3, second.Cursor)
	if !third.Reset || third.Output != "replacement screen" || third.Cursor == second.Cursor {
		t.Fatal("changed screen must replace, not append", third)
	}
	f.now = f.now.Add(61 * time.Second)
	if terminalCall(f.c, "alice", fmt.Sprintf(`{"action":"read","tid":%q,"seq":3}`, a.TID)).OK {
		t.Fatal("expired")
	}
}
func TestTerminalUnknownWriterDisabledAndIdentityReplacement(t *testing.T) {
	f := terminalFixtureNew(t)
	a := terminalPage(t, terminalCall(f.c, "a", `{"action":"open","mode":"rw"}`))
	f.unknown = true
	req := fmt.Sprintf(`{"action":"input","tid":%q,"seq":1,"data":"x"}`, a.TID)
	first := terminalCall(f.c, "a", req)
	if first.Outcome != "unknown" {
		t.Fatal(first)
	}
	if terminalCall(f.c, "a", req) != first {
		t.Fatal("unknown replay")
	}
	before := len(f.steps)
	if terminalCall(f.c, "a", strings.Replace(req, `"seq":1`, `"seq":2`, 1)).OK || len(f.steps) != before {
		t.Fatal("unknown writer reused")
	}
	if terminalCall(f.c, "b", `{"action":"open","mode":"rw"}`).OK {
		t.Fatal("unknown writer replaced")
	}
	f.proof = "session-b"
	if terminalCall(f.c, "a", fmt.Sprintf(`{"action":"read","tid":%q,"seq":2}`, a.TID)).OK {
		t.Fatal("replacement followed")
	}
}
func TestTerminalFiniteCountWriterTTLAndDisconnect(t *testing.T) {
	f := terminalFixtureNew(t)
	for i := 0; i < 4; i++ {
		terminalPage(t, terminalCall(f.c, "a", `{"action":"open"}`))
	}
	if terminalCall(f.c, "a", `{"action":"open"}`).OK {
		t.Fatal("count unbounded")
	}
	f.c.Close()
	if terminalCall(f.c, "a", `{"action":"open"}`).OK {
		t.Fatal("disconnected controller reusable")
	}
	f = terminalFixtureNew(t)
	terminalPage(t, terminalCall(f.c, "a", `{"action":"open","mode":"rw"}`))
	f.now = f.now.Add(31 * time.Second)
	terminalPage(t, terminalCall(f.c, "b", `{"action":"open","mode":"rw"}`))
	for _, args := range []string{`{"action":"open","cols":241}`, `{"action":"open","rows":0}`, `{"action":"open","mode":"x"}`, `{"action":"open","socket":"/tmp/socket"}`} {
		if terminalCall(f.c, "a", args).OK {
			t.Fatal(args)
		}
	}
}
