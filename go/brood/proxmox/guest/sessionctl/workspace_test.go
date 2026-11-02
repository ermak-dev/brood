//go:build linux

package sessionctl

// FEATURE: Fixed-clone workspace facts, wipe, recycle, and session restart.
// SPEC: Confirm effects, reject unsafe Git configuration and paths, preserve the
// clone, never confuse session liveness with cleanliness, never retry effects.
// IMPLEMENTATION: An injected workspace engine validates local Git metadata,
// isolates Git configuration, and re-proves session identity before effects.
// RESULT: IMPLEMENTED with real Git effects confined to temporary fixtures.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func workspaceRequest(op, args string) relay.SessionRequest {
	return relay.SessionRequest{Operation: op, Arguments: json.RawMessage(args)}
}
func workspaceFixture(t *testing.T) *workspaceEngine {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("/usr/bin/git", args...)
		c.Dir = root
		c.Env = workspaceGitEnv()
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %s %v", out, err)
		}
	}
	git("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture")
	return &workspaceEngine{root: root, git: func(ctx context.Context, args ...string) (string, error) { return runWorkspaceGit(ctx, root, args...) }, tmux: func(context.Context, ...string) (string, error) { return "$1\t%1\n", nil }, prove: func(context.Context) (identity, error) {
		return identity{PID: 123, SessionID: "old", Process: "start"}, nil
	}, quiet: func(context.Context) (bool, error) { return true, nil }}
}
func TestWorkspaceFactsAndWipe(t *testing.T) {
	e := workspaceFixture(t)
	facts := e.handle(context.Background(), workspaceRequest("workspace", `{"name":"actor"}`))
	if !facts.OK || !strings.Contains(facts.Text, `"git":"clean"`) {
		t.Fatalf("facts: %+v", facts)
	}
	os.WriteFile(filepath.Join(e.root, "tracked"), []byte("dirty"), 0600)
	os.WriteFile(filepath.Join(e.root, "build.out"), []byte("untracked"), 0600)
	facts = e.handle(context.Background(), workspaceRequest("workspace", `{}`))
	if !facts.OK || !strings.Contains(facts.Text, `"git":"dirty"`) {
		t.Fatalf("dirty running session: %+v", facts)
	}
	if r := e.handle(context.Background(), workspaceRequest("wipe", `{}`)); r.OK {
		t.Fatal("wipe without confirmation")
	}
	r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`))
	if !r.OK {
		t.Fatalf("wipe: %+v", r)
	}
	data, err := os.ReadFile(filepath.Join(e.root, "tracked"))
	if err != nil || string(data) != "original\n" {
		t.Fatalf("tracked: %q %v", data, err)
	}
	if _, err = os.Stat(filepath.Join(e.root, "build.out")); !os.IsNotExist(err) {
		t.Fatal("untracked output retained")
	}
	if _, err = os.Stat(filepath.Join(e.root, ".git")); err != nil {
		t.Fatal("clone removed")
	}
}
func TestWorkspaceRejectsUnsafeRepositories(t *testing.T) {
	for _, config := range []string{"[core]\nfsmonitor = /tmp/evil\n", "[filter \"evil\"]\nclean = /tmp/evil\n", "[include]\npath = /tmp/evil\n", "[core]\nworktree = /tmp\n", "[extensions]\nworktreeConfig = true\n"} {
		t.Run(config, func(t *testing.T) {
			e := workspaceFixture(t)
			f, err := os.OpenFile(filepath.Join(e.root, ".git/config"), os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString(config)
			f.Close()
			r := e.handle(context.Background(), workspaceRequest("workspace", `{}`))
			if !r.OK || !strings.Contains(r.Text, `"git":"unknown"`) {
				t.Fatalf("unsafe facts: %+v", r)
			}
			if r = e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); r.OK {
				t.Fatal("unsafe wipe")
			}
		})
	}
	t.Run("gitdir", func(t *testing.T) {
		e := workspaceFixture(t)
		os.Rename(filepath.Join(e.root, ".git"), filepath.Join(e.root, "actual"))
		os.WriteFile(filepath.Join(e.root, ".git"), []byte("gitdir: actual\n"), 0600)
		if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); r.OK {
			t.Fatal("gitdir accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		e := workspaceFixture(t)
		os.Rename(filepath.Join(e.root, ".git"), filepath.Join(e.root, "actual"))
		os.Symlink("actual", filepath.Join(e.root, ".git"))
		if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); r.OK {
			t.Fatal("symlink accepted")
		}
	})
	t.Run("non-git", func(t *testing.T) {
		e := workspaceFixture(t)
		os.RemoveAll(filepath.Join(e.root, ".git"))
		if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); r.OK {
			t.Fatal("non-git accepted")
		}
	})
}
func TestWorkspaceGuardsAndUnconfirmedEffects(t *testing.T) {
	for _, args := range []string{`{"confirm":true,"path":"/tmp"}`, `{"confirm":null}`, `{"confirm":true,"env":{}}`, `{"confirm":true,"executable":"evil"}`} {
		e := workspaceFixture(t)
		if r := e.handle(context.Background(), workspaceRequest("wipe", args)); r.OK {
			t.Fatalf("accepted %s", args)
		}
	}
	e := workspaceFixture(t)
	e.quiet = func(context.Context) (bool, error) { return false, nil }
	if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); r.OK {
		t.Fatal("live workspace wiped")
	}
	e.quiet = func(context.Context) (bool, error) { return false, errors.New("unknown") }
	if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); r.OK {
		t.Fatal("unknown workspace wiped")
	}
	e = workspaceFixture(t)
	calls := 0
	e.git = func(context.Context, ...string) (string, error) { calls++; return "", errors.New("lost") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := e.handle(ctx, workspaceRequest("wipe", `{"confirm":true}`))
	if r.OK || calls != 0 {
		t.Fatalf("canceled effects: %+v %d", r, calls)
	}
	e = workspaceFixture(t)
	realGit := e.git
	effects := 0
	e.git = func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "reset" {
			effects++
			return "", errors.New("lost")
		}
		return realGit(ctx, args...)
	}
	r = e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`))
	if r.OK || r.Outcome != "unknown" || effects != 1 {
		t.Fatalf("partial effect: %+v calls=%d", r, effects)
	}
}
func TestWorkspaceRestartAndRecycle(t *testing.T) {
	for _, op := range []string{"restart", "recycle"} {
		t.Run(op, func(t *testing.T) {
			e := workspaceFixture(t)
			os.WriteFile(filepath.Join(e.root, "CLAUDE.md"), []byte("rules"), 0600)
			var calls [][]string
			stopped := false
			e.tmux = func(_ context.Context, args ...string) (string, error) {
				calls = append(calls, append([]string(nil), args...))
				if args[0] == "list-panes" {
					return "$1\t%1\n", nil
				}
				if args[0] == "kill-session" {
					stopped = true
				}
				return "", nil
			}
			e.prove = func(context.Context) (identity, error) {
				if stopped {
					return identity{PID: 124, SessionID: "new", Process: "new-start"}, nil
				}
				return identity{PID: 123, SessionID: "old", Process: "old-start"}, nil
			}
			r := e.handle(context.Background(), workspaceRequest(op, `{"confirm":true}`))
			if !r.OK {
				t.Fatalf("%s: %+v", op, r)
			}
			if len(calls) != 3 || calls[0][0] != "list-panes" || calls[1][0] != "kill-session" || calls[1][2] != "$1" || calls[2][0] != "new-session" {
				t.Fatalf("tmux calls: %#v", calls)
			}
			launch := strings.Join(calls[2], " ")
			if strings.Contains(launch, "bypass") || !strings.Contains(launch, "/usr/local/bin/brood-guest --brood-guest-app") || strings.Contains(launch, "/usr/local/bin/claude") {
				t.Fatalf("launch: %s", launch)
			}
			_, err := os.Stat(filepath.Join(e.root, "CLAUDE.md"))
			if op == "restart" && err != nil {
				t.Fatal("restart removed instructions")
			}
			if op == "recycle" && !os.IsNotExist(err) {
				t.Fatal("recycle retained untracked instructions")
			}
		})
	}
	e := workspaceFixture(t)
	n := 0
	e.prove = func(context.Context) (identity, error) { n++; return identity{PID: n}, nil }
	calls := 0
	e.tmux = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "list-panes" {
			return "$1\t%1\n", nil
		}
		calls++
		return "", nil
	}
	r := e.handle(context.Background(), workspaceRequest("restart", `{"confirm":true}`))
	if r.OK || calls != 0 {
		t.Fatal("changed identity stopped")
	}
}
