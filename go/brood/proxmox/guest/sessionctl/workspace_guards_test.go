//go:build linux

package sessionctl

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest/ptysession"
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

func TestWorkspaceStopRefusesUnprovenHolder(t *testing.T) {
	e := workspaceFixture(t)
	calls := 0
	e.prove = func(context.Context) (identity, error) { return identity{PID: 123}, nil }
	e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
		calls++
		return liveReply(), nil
	}
	r := e.handle(context.Background(), workspaceRequest("restart", `{"confirm":true}`))
	if r.OK || calls != 0 {
		t.Fatalf("unproven holder stopped: %+v calls=%d", r, calls)
	}
}

func TestWorkspaceStopRefusalDoesNotCleanOrLaunch(t *testing.T) {
	e := workspaceFixture(t)
	effects := 0
	git := e.git
	e.git = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "reset" || args[0] == "clean" {
			t.Fatal("cleanup after refused stop")
		}
		return git(ctx, args...)
	}
	e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
		effects++
		if req.Action != "stop" || req.Proof != "holder" {
			t.Fatalf("unexpected effect: %+v", req)
		}
		return ptysession.Reply{Outcome: "refused", Error: "browser writer conflict"}, nil
	}
	r := e.handle(context.Background(), workspaceRequest("recycle", `{"confirm":true}`))
	if r.OK || r.Outcome == "unknown" || effects != 1 {
		t.Fatalf("refused stop: %+v effects=%d", r, effects)
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
			return identity{PID: 1, HolderProof: "holder"}, nil
		}
		return identity{PID: 2}, nil
	}
	e.call = func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
		if req.Action == "ensure" {
			cancel()
		}
		return liveReply(), nil
	}
	r := e.handle(ctx, workspaceRequest("restart", `{"confirm":true}`))
	if r.OK || r.Outcome != "unknown" {
		t.Fatalf("canceled launch reported success: %+v", r)
	}
}

func TestWorkspaceStatusFailureIsUnknown(t *testing.T) {
	e := workspaceFixture(t)
	stream := e.stream
	e.stream = func(ctx context.Context, args []string, consume func(*bufio.Reader) error) error {
		if args[0] == "status" {
			return errors.New("Git timed out")
		}
		return stream(ctx, args, consume)
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
