//go:build linux

package sessionctl

// FEATURE: Boot-time Claude session (#148).
// SPEC: The fixed detached tmux session claude starts with restart's launch
// arguments unless it exists; an existing session is never replaced, and a
// concurrent terminal attach that created it first counts as started.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeTMUX struct {
	calls   [][]string
	results []error
}

func (f *fakeTMUX) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if len(f.results) == 0 {
		return "", nil
	}
	err := f.results[0]
	f.results = f.results[1:]
	return "", err
}

func TestEnsureSessionStartsOnlyAMissingSession(t *testing.T) {
	has := []string{"has-session", "-t", "=claude"}
	missing := errors.New("no server running")
	for name, tc := range map[string]struct {
		results []error
		want    [][]string
		ok      bool
	}{
		"exists":         {nil, [][]string{has}, true},
		"missing":        {[]error{missing, nil}, [][]string{has, workspaceLaunchArgs()}, true},
		"attached first": {[]error{missing, errors.New("duplicate session"), nil}, [][]string{has, workspaceLaunchArgs(), has}, true},
		"failed":         {[]error{missing, errors.New("private"), missing}, [][]string{has, workspaceLaunchArgs(), has}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeTMUX{results: tc.results}
			err := ensureSession(context.Background(), f.run)
			if (err == nil) != tc.ok || (err != nil && strings.Contains(err.Error(), "private")) {
				t.Fatalf("result: %v", err)
			}
			if !reflect.DeepEqual(f.calls, tc.want) {
				t.Fatalf("tmux calls %q want %q", f.calls, tc.want)
			}
		})
	}
}

func TestEnsureSessionWithRealTMUX(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	root := t.TempDir()
	home, project := filepath.Join(root, "home"), filepath.Join(root, "project")
	for _, path := range []string{home, project} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	socket := filepath.Join(root, "tmux.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	// Тот же launch, что у restart, но на своём сокете и с безвредной командой.
	run := func(ctx context.Context, args ...string) (string, error) {
		rewritten := []string{"-S", socket, "-f", "/dev/null"}
		for _, arg := range args {
			switch arg {
			case fixedWorkspace:
				arg = project
			case "/usr/local/bin/brood-guest":
				arg = "/bin/sleep"
			case "--brood-guest-app":
				arg = "60"
			}
			rewritten = append(rewritten, arg)
		}
		return workspaceExec(ctx, home, env, "/usr/bin/tmux", rewritten...)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		workspaceExec(cleanup, home, env, "/usr/bin/tmux", "-S", socket, "kill-server")
	})
	pane := func() string {
		out, err := run(ctx, "display-message", "-p", "-t", "=claude:", "#{pane_pid}\t#{pane_current_path}")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	if err := ensureSession(ctx, run); err != nil {
		t.Fatal(err)
	}
	first := pane()
	if !strings.HasSuffix(first, "\t"+project) {
		t.Fatalf("pane cwd: %q", first)
	}
	if err := ensureSession(ctx, run); err != nil {
		t.Fatal(err)
	}
	if got := pane(); got != first {
		t.Fatalf("existing session replaced: %q then %q", first, got)
	}
	sessions, err := run(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil || strings.TrimSpace(sessions) != "claude" {
		t.Fatalf("sessions: %q %v", sessions, err)
	}
}
