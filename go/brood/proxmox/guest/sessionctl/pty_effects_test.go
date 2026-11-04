//go:build linux

package sessionctl

// FEATURE: Existing tools observe and control the real persistent PTY.
// SPEC: Tail reads rendered screen plus history, never raw escape logs; browser
// writer ownership refuses CLI input/resize/stop without stealing or restarting.

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
)

func TestTailWithRealRenderedScreenAndHistory(t *testing.T) {
	run, _ := realPTYFixture(t, []string{"/bin/sh", "-c", `printf 'stale\r\033[2Kfresh\n'; i=0; while [ "$i" -lt 35 ]; do printf 'history-%s\n' "$i"; i=$((i+1)); done; exec /bin/sleep 60`}, t.TempDir(), []string{"PATH=/usr/bin:/bin"})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := ensureSession(ctx, run); err != nil {
		t.Fatal(err)
	}
	holder, err := run(ctx, ptysession.Request{Action: "status"})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := fixture(t)
	e.call = run
	e.prove = func(context.Context) (identity, error) {
		id := fixtureIdentity()
		id.HolderProof = holder.Proof
		id.Epoch = holder.State.Epoch
		return id, nil
	}
	var text string
	if err = workspaceWait(ctx, 2*time.Second, func(ctx context.Context) (bool, error) {
		r := e.handle(ctx, workspaceRequest("tail", `{"lines":1000}`))
		text = r.Text
		return r.OK && strings.Contains(text, "history-34"), nil
	}); err != nil {
		t.Fatalf("screen never ready: %q err=%v", text, err)
	}
	if strings.Contains(text, "stale") || strings.ContainsAny(text, "\x1b\r") || !strings.Contains(text, "fresh") || !strings.Contains(text, "history-0\n") {
		t.Fatalf("not rendered history: %q", text)
	}
}

func TestRealBrowserWriterRejectsTerminalAndWorkspaceEffects(t *testing.T) {
	e := workspaceFixture(t)
	run, socket := realPTYFixture(t, []string{"/bin/sleep", "60"}, e.root, []string{"PATH=/usr/bin:/bin"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ensureSession(ctx, run); err != nil {
		t.Fatal(err)
	}
	holder, err := run(ctx, ptysession.Request{Action: "status"})
	if err != nil {
		t.Fatal(err)
	}
	id := fixtureIdentity()
	id.PID = holder.State.PID
	id.HolderProof = holder.Proof
	id.Epoch = holder.State.Epoch
	prove := func(context.Context) (identity, error) { return id, nil }
	connection, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(3 * time.Second))
	if err = ptysession.WriteMessage(connection, ptysession.Request{Action: "attach", Proof: holder.Proof, Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	var attached ptysession.Reply
	if err = ptysession.ReadMessage(bufio.NewReader(connection), &attached); err != nil || !attached.OK {
		t.Fatal(attached, err)
	}
	engine, _ := fixture(t)
	engine.call = run
	engine.prove = prove
	for _, action := range []string{"input", "interrupt", "resize"} {
		raw, _ := json.Marshal(TerminalStep{Action: action, Proof: terminalProof(id), Data: "unexpected", Cols: 90, Rows: 25})
		r := engine.handle(ctx, workspaceRequest("session_terminal", string(raw)))
		if r.OK || r.Outcome == "unknown" || !strings.Contains(r.Text, "writer conflict") {
			t.Fatalf("browser writer stolen by %s: %+v", action, r)
		}
	}
	e.call = run
	e.prove = prove
	r := e.handle(ctx, workspaceRequest("restart", `{"confirm":true}`))
	if r.OK || r.Outcome == "unknown" || !strings.Contains(r.Text, "writer conflict") {
		t.Fatalf("browser writer stolen by stop: %+v", r)
	}
	after, err := run(ctx, ptysession.Request{Action: "status"})
	if err != nil || !after.State.Running || after.State.PID != holder.State.PID || after.Proof != holder.Proof {
		t.Fatalf("writer process changed: %+v err=%v", after, err)
	}
}
