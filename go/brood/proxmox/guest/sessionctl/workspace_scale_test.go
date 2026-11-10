//go:build linux

package sessionctl

// FEATURE: Workspace operations on a clone of real size (#225).
// SPEC: An index listing, a status and the reports of reset and clean far
// over the helper's output cap neither refuse nor leave unknown workspace
// facts, restart, recycle and wipe.
// IMPLEMENTATION: Real Git on temporary fixtures; the PTY holder is replaced
// by a session that gets a new identity on every stop.
// RESULT: IMPLEMENTED with real Git effects confined to temporary fixtures.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest/ptysession"
)

// outputCap is the cap of cappedOutput; listings in these tests exceed it
// several times over.
const outputCap = 48 * 1024

// sessionGenerations lets restart and recycle prove a new session after
// every stop, however many operations run on e.
func sessionGenerations(e *workspaceEngine) {
	generation := 0
	e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
		if req.Action == "stop" {
			generation++
		}
		return liveReply(), nil
	}
	e.prove = func(context.Context) (identity, error) {
		return identity{PID: 100 + generation, SessionID: fmt.Sprint(generation), Process: "start", HolderProof: "holder"}, nil
	}
}

// plantUntracked writes n untracked files into the tracked top directory,
// so status and clean name each of them.
func plantUntracked(t *testing.T, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("untracked-build-output-%04d.tmp", i)), "output\n")
	}
}

func requireNoUntracked(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, "untracked-build-output-0000.tmp")); !os.IsNotExist(err) {
		t.Fatal("untracked output retained")
	}
}

// largeIndexFixture commits 3000 files; the brood clone itself lists about
// 61 KiB in 1601 files.
func largeIndexFixture(t *testing.T) *workspaceEngine {
	t.Helper()
	files := map[string]string{}
	for i := 0; i < 3000; i++ {
		files[fmt.Sprintf("module-%04d.go", i)] = "package project\n"
	}
	e := workspaceFixtureWith(t, actorHome, files)
	if index := fixtureGit(t, e.root, "ls-files", "--stage", "-z"); len(index) < 3*outputCap {
		t.Fatalf("index listing of %d bytes does not exceed the output cap enough", len(index))
	}
	return e
}

func TestScanIndex(t *testing.T) {
	entry := func(mode, path string) string { return mode + " " + strings.Repeat("a", 40) + " 0\t" + path + "\x00" }
	regular := entry("100644", "one") + entry("100755", "two") + entry("120000", "link")
	for _, c := range []struct {
		name, listing string
		limit         int
		refusal       string
	}{
		{"empty", "", 3, ""},
		{"regular", regular, 3, ""},
		{"gitlink", entry("100644", "one") + entry("160000", "sub") + entry("100644", "two"), 3, "submodule workspaces are not supported"},
		{"over limit", regular + entry("100644", "four"), 3, "Git index has more than 3 entries"},
		{"truncated", regular + "100644 aaaa", 4, "Git index listing is truncated"},
		{"long entry", entry("100644", strings.Repeat("p", 64)), 3, "Git index has an entry over 64 bytes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := scanIndex(bufio.NewReaderSize(strings.NewReader(c.listing), 64), c.limit)
			if c.refusal == "" && err != nil || c.refusal != "" && (err == nil || err.Error() != c.refusal) {
				t.Fatalf("refusal %v, want %q", err, c.refusal)
			}
		})
	}
}

func TestHelperGitOutputIsCapped(t *testing.T) {
	e := largeIndexFixture(t)
	if out, err := e.git(context.Background(), "ls-files", "--stage", "-z"); err == nil || out != "" {
		t.Fatalf("workspace Git passed %d bytes over the cap: %v", len(out), err)
	}
	f := newCheckoutFixture(t)
	plantUntracked(t, f.root, 3000)
	if out, err := f.engine.git(context.Background(), checkoutStepTimeout, nil, "ls-files", "--others", "-z"); err == nil || out != "" {
		t.Fatalf("checkout Git passed %d bytes over the cap: %v", len(out), err)
	}
}

func TestWorkspaceOperationsOnLargeIndex(t *testing.T) {
	e := largeIndexFixture(t)
	sessionGenerations(e)
	if r := e.handle(context.Background(), workspaceRequest("workspace", `{}`)); !r.OK || !strings.Contains(r.Text, `"git":"clean"`) {
		t.Fatalf("facts: %+v", r)
	}
	for _, op := range []string{"restart", "recycle", "wipe"} {
		if r := e.handle(context.Background(), workspaceRequest(op, `{"confirm":true}`)); !r.OK {
			t.Fatalf("%s: %+v", op, r)
		}
	}
}

func TestWorkspaceOperationsOnLargeChanges(t *testing.T) {
	e := workspaceFixture(t)
	sessionGenerations(e)
	plantUntracked(t, e.root, 3000)
	if r := e.handle(context.Background(), workspaceRequest("workspace", `{}`)); !r.OK || !strings.Contains(r.Text, `"git":"dirty"`) {
		t.Fatalf("facts: %+v", r)
	}
	for _, op := range []string{"wipe", "recycle"} {
		plantUntracked(t, e.root, 3000)
		if r := e.handle(context.Background(), workspaceRequest(op, `{"confirm":true}`)); !r.OK {
			t.Fatalf("%s: %+v", op, r)
		}
		requireNoUntracked(t, e.root)
	}
	// reset --hard называет subject нового HEAD.
	fixtureGit(t, e.root, "commit", "--quiet", "--allow-empty", "-m", strings.Repeat("s", 2*outputCap))
	writeFile(t, filepath.Join(e.root, "tracked"), "changed\n")
	if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); !r.OK {
		t.Fatalf("wipe under a long subject: %+v", r)
	}
	if readFile(t, filepath.Join(e.root, "tracked")) != "original\n" {
		t.Fatal("wipe did not restore the commit")
	}
}
