//go:build linux

package sessionctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceTraversalDepthBound(t *testing.T) {
	e := workspaceFixture(t)
	path := e.root
	for i := 0; i < 130; i++ {
		path = filepath.Join(path, "d")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.repository(context.Background()); err == nil {
		t.Fatal("unbounded workspace depth accepted")
	}
}

func TestWorkspaceStopRefusesAdditionalPanes(t *testing.T) {
	e := workspaceFixture(t)
	killed := false
	e.tmux = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "list-panes" {
			return "$1\t%1\n$1\t%2\n", nil
		}
		if args[0] == "kill-session" {
			killed = true
		}
		return "", nil
	}
	r := e.handle(context.Background(), workspaceRequest("restart", `{"confirm":true}`))
	if r.OK || killed {
		t.Fatalf("unproven additional panes stopped: %+v killed=%v", r, killed)
	}
}
func TestWorkspaceCancellationAfterLaunchIsUnconfirmed(t *testing.T) {
	e := workspaceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proofs := 0
	e.prove = func(context.Context) (identity, error) {
		proofs++
		if proofs < 3 {
			return identity{PID: 1}, nil
		}
		return identity{PID: 2}, nil
	}
	e.tmux = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "list-panes" {
			return "$1\t%1\n", nil
		}
		if args[0] == "new-session" {
			cancel()
		}
		return "", nil
	}
	r := e.handle(ctx, workspaceRequest("restart", `{"confirm":true}`))
	if r.OK || r.Outcome != "unknown" {
		t.Fatalf("canceled launch reported success: %+v", r)
	}
}

func TestWorkspaceStatusFailureIsUnknown(t *testing.T) {
	e := workspaceFixture(t)
	git := e.git
	e.git = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "status" {
			return "", errors.New("limit")
		}
		return git(ctx, args...)
	}
	r := e.handle(context.Background(), workspaceRequest("workspace", `{}`))
	if !r.OK || !strings.Contains(r.Text, `"git":"unknown"`) {
		t.Fatalf("unknown status: %+v", r)
	}
}
func TestWorkspaceHooksAndIgnoredOutput(t *testing.T) {
	e := workspaceFixture(t)
	if err := os.WriteFile(filepath.Join(e.root, ".git/hooks/post-index-change"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, ".gitignore"), []byte("output/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.root, "output"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, "output/build"), []byte("ignored output"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); !r.OK {
		t.Fatalf("safe hooks override: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.root, "output")); !os.IsNotExist(err) {
		t.Fatal("ignored output retained")
	}
}
