//go:build linux

package sessionctl

// FEATURE: Session restart in a workspace that is no Git checkout (#158).
// SPEC: Restart keeps the workspace and runs no Git effect, so it does not
// require a checkout: an actor without origin starts with a plain directory,
// and the session itself may leave a repository that wipe refuses. Restart
// keeps every other proof: the fixed directory, a live session with its holder
// proof, a quiet workspace before launch, an unknown outcome after any
// unconfirmed effect. Wipe and recycle clean the clone and still refuse such a
// workspace before the session stops.
// IMPLEMENTATION: Real fixtures in a temporary directory; the PTY holder is
// replaced by a session that gets a new identity once launched.
// RESULT: IMPLEMENTED with real Git confined to temporary fixtures.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
)

// notCheckouts are workspaces that pass no repository proof, with the reason
// that wipe and recycle give for each.
var notCheckouts = []struct {
	name   string
	plant  func(t *testing.T, root string)
	reason string
}{
	{"no-git", removeGitDir, "must be a Git clone"},
	{"unborn-head", func(t *testing.T, root string) {
		removeGitDir(t, root)
		fixtureGit(t, root, "init", "--quiet", "--initial-branch=main")
	}, "HEAD is not a local commit"},
	{"unsupported-remote", func(t *testing.T, root string) {
		fixtureGit(t, root, "remote", "add", "upstream", "https://git.example.invalid/group/repo.git")
	}, "unsupported repository Git configuration"},
}

func removeGitDir(t *testing.T, root string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
}

// launchingSession replaces the PTY holder: the old session holds the proof
// "holder" until ensure launches one with a new identity.
func launchingSession(e *workspaceEngine) {
	launched := false
	e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
		if req.Action == "ensure" {
			launched = true
		}
		return liveReply(), nil
	}
	e.prove = func(context.Context) (identity, error) {
		if launched {
			return identity{PID: 124, SessionID: "new", Process: "new-start"}, nil
		}
		return identity{PID: 123, SessionID: "old", Process: "old-start", HolderProof: "holder"}, nil
	}
}

// holderActions records the actions e sends to the PTY holder.
func holderActions(e *workspaceEngine) *[]string {
	var actions []string
	call := e.call
	e.call = func(ctx context.Context, req ptysession.Request) (ptysession.Reply, error) {
		actions = append(actions, req.Action)
		return call(ctx, req)
	}
	return &actions
}

// refuseGitEffects fails the test on any Git command that changes the clone.
func refuseGitEffects(t *testing.T, e *workspaceEngine) {
	git := e.git
	e.git = func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && (args[0] == "reset" || args[0] == "clean") {
			t.Errorf("Git effect in %s: %v", e.root, args)
		}
		return git(ctx, args...)
	}
}

func TestWorkspaceRestartWithoutCheckout(t *testing.T) {
	for _, c := range notCheckouts {
		t.Run(c.name, func(t *testing.T) {
			e := workspaceFixture(t)
			c.plant(t, e.root)
			writeFile(t, filepath.Join(e.root, "notes"), "work in progress\n")
			launchingSession(e)
			refuseGitEffects(t, e)
			actions := holderActions(e)
			r := e.handle(context.Background(), workspaceRequest("restart", `{"confirm":true}`))
			if !r.OK || !strings.Contains(r.Text, `"workspace_preserved":true`) {
				t.Fatalf("restart without checkout: %+v", r)
			}
			if strings.Join(*actions, ",") != "stop,ensure" {
				t.Fatalf("holder actions: %v", *actions)
			}
			if data, err := os.ReadFile(filepath.Join(e.root, "notes")); err != nil || string(data) != "work in progress\n" {
				t.Fatalf("restart changed the workspace: %q %v", data, err)
			}
		})
	}
}

// replaceWithSymlink swaps the fixed directory for a symlink to its content.
func replaceWithSymlink(t *testing.T, root string) {
	t.Helper()
	if err := os.Rename(root, root+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root+".real", root); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceRestartWithoutCheckoutKeepsProofs(t *testing.T) {
	old := identity{PID: 123, SessionID: "old", Process: "old-start", HolderProof: "holder"}
	cases := []struct {
		name    string
		setup   func(t *testing.T, e *workspaceEngine)
		outcome string
		actions string
		reason  string
	}{
		{"symlinked-workspace", func(t *testing.T, e *workspaceEngine) { replaceWithSymlink(t, e.root) }, "failed", "", "fixed workspace is unavailable or unsafe"},
		{"missing-workspace", func(t *testing.T, e *workspaceEngine) { os.RemoveAll(e.root) }, "failed", "", "fixed workspace is unavailable or unsafe"},
		{"no-live-session", func(t *testing.T, e *workspaceEngine) {
			e.prove = func(context.Context) (identity, error) { return identity{}, errors.New("no session") }
		}, "failed", "", "no proven live Claude session"},
		{"no-holder-proof", func(t *testing.T, e *workspaceEngine) {
			e.prove = func(context.Context) (identity, error) { return identity{PID: 123}, nil }
		}, "failed", "", "holder proof unavailable"},
		{"session-changed", func(t *testing.T, e *workspaceEngine) {
			n := 0
			e.prove = func(context.Context) (identity, error) { n++; return identity{PID: n, HolderProof: "holder"}, nil }
		}, "failed", "", "live session changed"},
		{"stop-refused", func(t *testing.T, e *workspaceEngine) {
			e.call = func(context.Context, ptysession.Request) (ptysession.Reply, error) {
				return ptysession.Reply{Outcome: "refused", Error: "browser writer conflict"}, nil
			}
		}, "failed", "stop", "browser writer conflict"},
		{"stop-unknown", func(t *testing.T, e *workspaceEngine) {
			e.call = func(context.Context, ptysession.Request) (ptysession.Reply, error) {
				return ptysession.Reply{}, errors.New("holder connection lost")
			}
		}, "unknown", "stop", "session stop result is unknown"},
		{"inventory-unknown", func(t *testing.T, e *workspaceEngine) {
			e.quiet = func(context.Context) (bool, error) { return false, errors.New("inventory unavailable") }
		}, "unknown", "stop", "not yet confirmed"},
		{"foreign-process-before-launch", func(t *testing.T, e *workspaceEngine) {
			scans := 0
			e.quiet = func(context.Context) (bool, error) { scans++; return scans == 1, nil }
		}, "unknown", "stop", "session stopped; workspace is live"},
		{"workspace-lost-while-waiting", func(t *testing.T, e *workspaceEngine) {
			moved := false
			e.quiet = func(context.Context) (bool, error) {
				if !moved {
					moved = true
					replaceWithSymlink(t, e.root)
				}
				return true, nil
			}
		}, "unknown", "stop", "fixed workspace proof was lost"},
		{"new-session-unproven", func(t *testing.T, e *workspaceEngine) {
			e.prove = func(context.Context) (identity, error) { return old, nil }
		}, "unknown", "stop,ensure", "not yet proven"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := workspaceFixture(t)
			removeGitDir(t, e.root)
			launchingSession(e)
			c.setup(t, e)
			refuseGitEffects(t, e)
			actions := holderActions(e)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			r := e.handle(ctx, workspaceRequest("restart", `{"confirm":true}`))
			if r.OK || r.Outcome != c.outcome || !strings.Contains(r.Text, c.reason) || strings.Join(*actions, ",") != c.actions {
				t.Fatalf("restart without checkout: %+v holder=%v", r, *actions)
			}
		})
	}
}
