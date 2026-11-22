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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest/ptysession"
	"brood.dev/go/brood/proxmox/relay"
)

func fixtureEnv(home string) []string {
	return []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
}

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("/usr/bin/git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "protocol.file.allow=always"}, args...)...)
	c.Dir = dir
	c.Env = fixtureEnv(dir)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("fixture git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func workspaceRequest(op, args string) relay.SessionRequest {
	return relay.SessionRequest{Operation: op, Arguments: json.RawMessage(args)}
}
func workspaceFixture(t *testing.T) *workspaceEngine {
	return workspaceFixtureWith(t, actorHome, nil)
}

// workspaceFixtureWith commits files next to tracked; helper Git runs with
// home as HOME.
func workspaceFixtureWith(t *testing.T, home string, files map[string]string) *workspaceEngine {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("/usr/bin/git", args...)
		c.Dir = root
		c.Env = gitEnv(actorHome, root)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %s %v", out, err)
		}
	}
	git("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "--all")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture")
	return &workspaceEngine{root: root, git: func(ctx context.Context, args ...string) (string, error) {
		return runWorkspaceGit(ctx, home, root, args...)
	}, stream: func(ctx context.Context, args []string, consume func(*bufio.Reader) error) error {
		return streamWorkspaceGit(ctx, home, root, args, consume)
	}, call: func(context.Context, ptysession.Request) (ptysession.Reply, error) { return liveReply(), nil }, prove: func(context.Context) (identity, error) {
		return identity{PID: 123, SessionID: "old", Process: "start", HolderProof: "holder"}, nil
	}, quiet: func(context.Context) (bool, error) { return true, nil }}
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
	t.Run("gitlink", func(t *testing.T) {
		e := workspaceFixture(t)
		fixtureGit(t, e.root, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",sub")
		if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); r.OK || !strings.Contains(r.Text, "submodule") {
			t.Fatalf("gitlink accepted: %+v", r)
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

// FEATURE: The first-start clone records its canonical origin (#147).
// SPEC: brood.origin is plain data, so restart/recycle/wipe keep accepting the
// clone; other unknown keys stay refused.
func TestWorkspaceAcceptsRecordedOrigin(t *testing.T) {
	e := workspaceFixture(t)
	f, err := os.OpenFile(filepath.Join(e.root, ".git/config"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("[brood]\n\torigin = https://git.example.org/group/repo.git\n[remote \"origin\"]\n\turl = http://127.0.0.1:8318/origin.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")
	f.Close()
	if r := e.handle(context.Background(), workspaceRequest("workspace", `{}`)); !r.OK || !strings.Contains(r.Text, `"git":"clean"`) {
		t.Fatalf("recorded origin refused: %+v", r)
	}
	if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); !r.OK {
		t.Fatalf("wipe of a recorded clone: %+v", r)
	}
	if workspaceConfigKey("brood.other") {
		t.Fatal("only the recorded origin is accepted in the brood section")
	}
}

// SPEC (#249): workspace, wipe and recycle do not know the installation
// identity, so user.name and user.email stay allowlisted as data with any
// value, exactly as since #125; only the checkout enforces the value.
func TestWorkspaceKeepsAnyRecordedUserIdentity(t *testing.T) {
	e := workspaceFixture(t)
	fixtureGit(t, e.root, "config", "user.name", "Someone Else")
	fixtureGit(t, e.root, "config", "user.email", "someone@example.invalid")
	if r := e.handle(context.Background(), workspaceRequest("workspace", `{}`)); !r.OK || !strings.Contains(r.Text, `"git":"clean"`) {
		t.Fatalf("user identity refused: %+v", r)
	}
	if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); !r.OK {
		t.Fatalf("wipe with user identity: %+v", r)
	}
	if got := fixtureGit(t, e.root, "config", "--file", filepath.Join(e.root, ".git/config"), "user.email"); got != "someone@example.invalid" {
		t.Fatalf("wipe rewrote the identity: %q", got)
	}
}
