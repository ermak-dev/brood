//go:build linux

package sessionctl

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceTransitionWaitsForTeardownAndNewProof(t *testing.T) {
	for _, op := range []string{"restart", "recycle"} {
		t.Run(op, func(t *testing.T) {
			e := workspaceFixture(t)
			stopped, launched := false, false
			quietCalls, newProofs, resets := 0, 0, 0
			old := identity{PID: 123, SessionID: "old", Process: "old-start"}
			e.prove = func(context.Context) (identity, error) {
				if !launched {
					return old, nil
				}
				newProofs++
				switch newProofs {
				case 1:
					return identity{}, errors.New("record not ready")
				case 2:
					return old, nil
				default:
					return identity{PID: 124, SessionID: "new", Process: "new-start"}, nil
				}
			}
			e.quiet = func(context.Context) (bool, error) {
				if !stopped || launched {
					t.Fatal("quiet scan outside stop/launch boundary")
				}
				quietCalls++
				return quietCalls >= 3, nil
			}
			e.tmux = func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "list-panes":
					return "$1\t%1\n", nil
				case "kill-session":
					if stopped {
						t.Fatal("stop repeated")
					}
					stopped = true
				case "new-session":
					if launched || quietCalls < 3 {
						t.Fatal("launch before quiet or repeated")
					}
					launched = true
				}
				return "", nil
			}
			git := e.git
			e.git = func(ctx context.Context, args ...string) (string, error) {
				if args[0] == "reset" || args[0] == "clean" {
					if quietCalls < 3 {
						t.Fatal("cleanup before old processes stopped")
					}
					resets++
				}
				return git(ctx, args...)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			r := e.handle(ctx, workspaceRequest(op, `{"confirm":true}`))
			if !r.OK || !launched || newProofs != 3 || (op == "recycle" && resets != 2) {
				t.Fatalf("delayed transition: %+v launched=%v proofs=%d cleanup=%d", r, launched, newProofs, resets)
			}
		})
	}
}

func TestWorkspaceTransitionWaitsForNewProof(t *testing.T) {
	e := workspaceFixture(t)
	launched, proofs := false, 0
	e.tmux = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "new-session" {
			launched = true
		}
		return "$1\t%1\n", nil
	}
	e.prove = func(context.Context) (identity, error) {
		if !launched {
			return identity{PID: 123, SessionID: "old"}, nil
		}
		proofs++
		if proofs < 3 {
			return identity{}, errors.New("new session not ready")
		}
		return identity{PID: 124, SessionID: "new"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := e.handle(ctx, workspaceRequest("restart", `{"confirm":true}`))
	if !r.OK || proofs != 3 {
		t.Fatalf("delayed proof: %+v attempts=%d", r, proofs)
	}
}

func TestWorkspaceTransitionTimeoutAndCancellation(t *testing.T) {
	for _, phase := range []string{"stop", "proof", "unchanged-proof"} {
		for _, cancelWait := range []bool{false, true} {
			t.Run(phase+"/cancel="+strconv.FormatBool(cancelWait), func(t *testing.T) {
				e := workspaceFixture(t)
				ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
				defer cancel()
				stops, launches, attempts := 0, 0, 0
				old := identity{PID: 123, SessionID: "old", Process: "old-start"}
				pending := func() {
					attempts++
					if cancelWait && attempts == 2 {
						cancel()
					}
				}
				e.quiet = func(context.Context) (bool, error) {
					if phase != "stop" {
						return true, nil
					}
					pending()
					return false, nil
				}
				e.prove = func(context.Context) (identity, error) {
					if launches == 0 {
						return old, nil
					}
					pending()
					if phase == "unchanged-proof" {
						return old, nil
					}
					return identity{}, errors.New("not proven")
				}
				e.tmux = func(_ context.Context, args ...string) (string, error) {
					switch args[0] {
					case "list-panes":
						return "$1\t%1\n", nil
					case "kill-session":
						stops++
					case "new-session":
						launches++
					}
					return "", nil
				}
				start := time.Now()
				r := e.handle(ctx, workspaceRequest("restart", `{"confirm":true}`))
				wantLaunches := 1
				if phase == "stop" {
					wantLaunches = 0
				}
				if r.OK || r.Outcome != "unknown" || ctx.Err() == nil || attempts < 2 || stops != 1 || launches != wantLaunches || time.Since(start) > time.Second {
					t.Fatalf("unconfirmed %s: %+v ctx=%v attempts=%d stops=%d launches=%d elapsed=%v", phase, r, ctx.Err(), attempts, stops, launches, time.Since(start))
				}
			})
		}
	}
}

func TestWorkspaceTransitionRechecksRepositoryAfterWaiting(t *testing.T) {
	e := workspaceFixture(t)
	quietCalls, launches := 0, 0
	e.quiet = func(context.Context) (bool, error) {
		quietCalls++
		if quietCalls == 1 {
			return false, nil
		}
		if quietCalls == 2 {
			if err := os.Rename(filepath.Join(e.root, ".git"), filepath.Join(e.root, "moved")); err != nil {
				t.Fatal(err)
			}
		}
		return true, nil
	}
	e.tmux = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "new-session" {
			launches++
		}
		return "$1\t%1\n", nil
	}
	r := e.handle(context.Background(), workspaceRequest("restart", `{"confirm":true}`))
	if r.OK || r.Outcome != "unknown" || quietCalls < 2 || launches != 0 || !strings.Contains(r.Text, "repository proof was lost") {
		t.Fatalf("repository changed while waiting: %+v quiet=%d launches=%d", r, quietCalls, launches)
	}
}

func TestWorkspaceTransitionInventoryErrorDoesNotLaunch(t *testing.T) {
	e := workspaceFixture(t)
	launches, scans := 0, 0
	e.quiet = func(context.Context) (bool, error) { scans++; return false, errors.New("inventory unavailable") }
	e.tmux = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "new-session" {
			launches++
		}
		return "$1\t%1\n", nil
	}
	r := e.handle(context.Background(), workspaceRequest("restart", `{"confirm":true}`))
	if r.OK || r.Outcome != "unknown" || launches != 0 || scans != 1 {
		t.Fatalf("unknown inventory: %+v launches=%d scans=%d", r, launches, scans)
	}
}

func TestWorkspaceTMUXControlCWDContract(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "workspace.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runWorkspaceTMUX" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok || name.Name != "workspaceExec" {
				return true
			}
			found = true
			cwd, ok := call.Args[1].(*ast.BasicLit)
			if !ok || cwd.Value != strconv.Quote("/workspace/home") {
				t.Error("tmux control/server must start in fixed /workspace/home, not the clone")
			}
			return true
		})
	}
	if !found {
		t.Fatal("workspace tmux command not inspected")
	}
	args := workspaceLaunchArgs()
	if len(args) < 6 || args[4] != "-c" || args[5] != fixedWorkspace {
		t.Fatalf("pane must still start in project: %q", args)
	}
}

func TestWorkspaceTMUXServerOutsidePaneCWD(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run := func(args ...string) (string, error) {
		return workspaceExec(ctx, home, []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}, "/usr/bin/tmux", append([]string{"-S", socket, "-f", "/dev/null"}, args...)...)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		workspaceExec(cleanup, home, []string{"PATH=/usr/bin:/bin"}, "/usr/bin/tmux", "-S", socket, "kill-server")
	})
	if _, err := run("new-session", "-d", "-s", "fixture", "-c", project, "/bin/sleep", "60"); err != nil {
		t.Fatal(err)
	}
	out, err := run("display-message", "-p", "-t", "fixture", "#{pid}\t#{pane_current_path}")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSpace(out), "\t")
	if len(parts) != 2 || parts[1] != project {
		t.Fatalf("pane cwd: %q", out)
	}
	serverCWD, err := os.Readlink("/proc/" + parts[0] + "/cwd")
	if err != nil || serverCWD != home {
		t.Fatalf("server cwd=%q want=%q err=%v", serverCWD, home, err)
	}
}
