//go:build linux

package sessionctl

// FEATURE: Boot-time Claude session uses the persistent PTY holder (#162).
// SPEC: Ensure sends only a fixed ensure request, never a command/environment;
// repeated ensure preserves the same live PID, start, epoch and project cwd.
// IMPLEMENTATION: The root helper delegates process creation to the PTY host.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
)

func TestEnsureSessionStartsOnlyAMissingSession(t *testing.T) {
	for _, kind := range []string{"exists", "missing", "failed", "not-running", "no-proof", "transport"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			run := func(_ context.Context, req ptysession.Request) (ptysession.Reply, error) {
				calls++
				if req.Action != "ensure" || req.Proof != "" || len(req.Data) != 0 || req.Columns != 0 || req.Rows != 0 {
					t.Fatalf("non-fixed ensure: %+v", req)
				}
				reply := liveReply()
				switch kind {
				case "failed":
					reply.OK = false
					reply.Error = "private"
				case "not-running":
					reply.State.Running = false
				case "no-proof":
					reply.Proof = ""
				case "transport":
					return ptysession.Reply{}, errors.New("private")
				}
				return reply, nil
			}
			err := ensureSession(context.Background(), run)
			want := kind == "exists" || kind == "missing"
			if (err == nil) != want || calls != 1 || (err != nil && strings.Contains(err.Error(), "private")) {
				t.Fatalf("ensure: %v calls=%d", err, calls)
			}
		})
	}
}

func realPTYFixture(t *testing.T, command []string, dir string, env []string) (ptyCall, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "brood-pty-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	socket := filepath.Join(root, "pty.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(socket, 0660); err != nil {
		t.Fatal(err)
	}
	host, err := ptysession.New(ptysession.Config{Command: command, Dir: dir, Env: env, Columns: 80, Rows: 24})
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		host.Close()
		listener.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("PTY server did not stop")
		}
	})
	run := func(ctx context.Context, req ptysession.Request) (ptysession.Reply, error) {
		return trustedPTYCall(ctx, socket, uint32(os.Getuid()), uint32(os.Getgid()), root, req)
	}
	return run, socket
}

func TestEnsureSessionWithRealPTY(t *testing.T) {
	project := t.TempDir()
	controllerCWD, _ := os.Getwd()
	run, _ := realPTYFixture(t, []string{"/bin/sleep", "60"}, project, []string{"PATH=/usr/bin:/bin"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ensureSession(ctx, run); err != nil {
		t.Fatal(err)
	}
	first, err := run(ctx, ptysession.Request{Action: "status"})
	if err != nil || !first.OK {
		t.Fatal(first, err)
	}
	childCWD, err := os.Readlink("/proc/" + strconv.Itoa(first.State.PID) + "/cwd")
	if err != nil || childCWD != project {
		t.Fatalf("child cwd=%q err=%v", childCWD, err)
	}
	hostCWD, _ := os.Getwd()
	if hostCWD != controllerCWD {
		t.Fatalf("controller cwd changed: %q", hostCWD)
	}
	if err := ensureSession(ctx, run); err != nil {
		t.Fatal(err)
	}
	second, err := run(ctx, ptysession.Request{Action: "status"})
	if err != nil || first.State != second.State || first.Proof != second.Proof {
		t.Fatalf("existing session replaced: %+v %+v err=%v", first, second, err)
	}
}
