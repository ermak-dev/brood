//go:build linux

package guest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestActorEnvironmentAndPayload(t *testing.T) {
	t.Setenv("BROOD_GUEST_TOKEN", "private-token")
	t.Setenv("ROOT_SECRET", "private-root")
	t.Setenv("ANTHROPIC_API_KEY", "private-key")
	t.Setenv("PATH", "/evil")
	spec := actorSpec("/trusted/guest")
	joined := strings.Join(spec.Env, "\n")
	for _, forbidden := range []string{"BROOD_GUEST_", "private-", "/evil", "ANTHROPIC_API_KEY", "PROXY=", "proxy="} {
		if strings.Contains(joined, forbidden) {
			t.Fatal(joined)
		}
	}
	for _, want := range []string{"HOME=/workspace/home", "PWD=/workspace/project"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s: %s", want, joined)
		}
	}
	if spec.UID != 1000 || spec.GID != 1000 || spec.Dir != "/workspace/project" || !reflect.DeepEqual(spec.Args, []string{"--brood-guest-actor"}) {
		t.Fatalf("%+v", spec)
	}
	// FEATURE: #160: driftty заменяет только браузерный клиент ttyd.
	// SPEC: Индекс выбран явно, когда файл есть; без него остаётся ttyd UI.
	// Loopback, origin check и общая tmux-сессия не меняются.
	// IMPLEMENTATION: Флаг наличия index добавляет только фиксированную опцию ttyd.
	// RESULT: IMPLEMENTED — оба поколения образов сохраняют прежний runtime.
	// STATUS: DONE
	for _, driftty := range []bool{false, true} {
		want := []string{"--interface", "127.0.0.1", "--port", "8080", "--writable", "--check-origin"}
		if driftty {
			want = append(want, "--index", "/usr/local/share/brood/driftty/index.html")
		}
		want = append(want, "/opt/brood/bin/brood-guest", "--brood-guest-attach")
		if got := actorArgs("/opt/brood/bin/brood-guest", driftty); !reflect.DeepEqual(got, want) {
			t.Fatalf("driftty=%t: got %q, want %q", driftty, got, want)
		}
	}
}

func privateTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWorkspaceEscape(t *testing.T) {
	for _, name := range []string{"home", "project"} {
		t.Run(name, func(t *testing.T) {
			root := privateTemp(t)
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			if err := prepareWorkspace(root, os.Getuid(), os.Getgid()); err == nil {
				t.Fatal("workspace symlink accepted")
			}
			items, _ := os.ReadDir(outside)
			if len(items) != 0 {
				t.Fatal("outside modified")
			}
		})
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace")
	_ = os.Symlink(real, link)
	if err := prepareWorkspace(link, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("workspace root symlink accepted")
	}
	root := privateTemp(t)
	if err := prepareWorkspace(root, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "home", "keep")
	_ = os.WriteFile(marker, []byte("persistent"), 0600)
	if err := prepareWorkspace(root, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(marker)
	if string(b) != "persistent" {
		t.Fatal("persistent data lost")
	}
}

func TestNoNewPrivilegesSubprocess(t *testing.T) {
	if os.Getenv("GUEST_PRIVILEGE_TEST") == "1" {
		if err := lockPrivileges(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		status, _ := os.ReadFile("/proc/self/status")
		for _, want := range []string{"NoNewPrivs:\t1", "CapEff:\t0000000000000000", "CapAmb:\t0000000000000000"} {
			if !bytes.Contains(status, []byte(want)) {
				os.Exit(3)
			}
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNoNewPrivilegesSubprocess$")
	cmd.Env = append(os.Environ(), "GUEST_PRIVILEGE_TEST=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

type fakeProcess struct {
	done   chan struct{}
	once   sync.Once
	events *[]string
}

func (p *fakeProcess) Wait() error { <-p.done; return nil }
func (p *fakeProcess) Close() error {
	p.once.Do(func() { *p.events = append(*p.events, "process-close"); close(p.done) })
	return nil
}

func testConfig(events *[]string) Config {
	return Config{Executable: "/trusted/guest", Dependencies: &Dependencies{EUID: func() int { return 0 }, Workspace: func() error { *events = append(*events, "workspace"); return nil }, Launch: func(context.Context, ActorSpec) (Process, error) {
		*events = append(*events, "process-start")
		return &fakeProcess{done: make(chan struct{}), events: events}, nil
	}}}
}
func TestLifecycle(t *testing.T) {
	var events []string
	c := testConfig(&events)
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if h.TerminalDialTarget() != "127.0.0.1:8080" {
		t.Fatal(h.TerminalDialTarget())
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	want := []string{"workspace", "process-start", "process-close"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("%v", events)
	}
}
func TestStartupFailsClosed(t *testing.T) {
	for _, stage := range []string{"nonroot", "workspace", "launch", "missing"} {
		t.Run(stage, func(t *testing.T) {
			var events []string
			c := testConfig(&events)
			switch stage {
			case "nonroot":
				c.Dependencies.EUID = func() int { return 1000 }
			case "workspace":
				c.Dependencies.Workspace = func() error { return errors.New("unsafe workspace") }
			case "launch":
				c.Dependencies.Launch = func(context.Context, ActorSpec) (Process, error) { return nil, errors.New("exec failed") }
			case "missing":
				c.Dependencies = nil
			}
			h, err := Start(context.Background(), c)
			if err == nil || h != nil {
				t.Fatal("started despite failure")
			}
			if stage != "launch" && strings.Contains(strings.Join(events, ","), "process-start") {
				t.Fatal(events)
			}
		})
	}
}
