//go:build linux

package main

// FEATURE: Bootstrap guest binaries from verified server artifacts (#155).
// SPEC: The root supervisor stage splits: bootstrap installs the three
// verified artifacts into /opt/brood/bin and execs the runtime stage before
// any network service; runtime prepends /opt/brood/bin to the helper PATH and
// never fetches. BROOD_GUEST_STAGE is process env only, never a PVE variable:
// unknown values are rejected by name. Install is atomic temp-then-rename and
// refuses changed content over an existing binary.
// IMPLEMENTATION: Stage field in config, artifacts/reexec dependencies with
// fakes here, real atomic installer under a redirected artifact root.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func stageEnv(t *testing.T, stage string) []string {
	t.Helper()
	env := validEnv(t)
	if stage == "" {
		return env
	}
	return append(env, "BROOD_GUEST_STAGE="+stage)
}

func TestConfigStage(t *testing.T) {
	for _, valid := range []string{"", "bootstrap", "runtime"} {
		c, err := parseConfig(stageEnv(t, valid))
		if err != nil {
			t.Fatalf("stage %q rejected: %v", valid, err)
		}
		if want := map[string]string{"": "bootstrap", "bootstrap": "bootstrap", "runtime": "runtime"}[valid]; c.stage != want {
			t.Fatalf("stage %q parsed as %q", valid, c.stage)
		}
	}
	for _, invalid := range []string{"Bootstrap", "BOOTSTRAP", "boot", "runtime ", " production", "test"} {
		if _, err := parseConfig(stageEnv(t, invalid)); err == nil || !strings.Contains(err.Error(), "BROOD_GUEST_STAGE") {
			t.Fatalf("stage %q accepted: %v", invalid, err)
		}
	}
	if _, err := parseConfig(append(stageEnv(t, "runtime"), "BROOD_GUEST_STAGE=bootstrap")); err == nil {
		t.Fatal("duplicate stage accepted")
	}
}

// supervisorDeps собирает корневой supervisor с подменами и журналом событий;
// не заданные зависимости отказываются вызываться.
func supervisorDeps(t *testing.T, events *[]string, stop func()) dependencies {
	t.Helper()
	network, runtime := newFake(), newFake()
	return dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) {
		*events = append(*events, "network")
		return network, nil
	}, start: func(context.Context, []string) (actorRuntime, error) {
		*events = append(*events, "services")
		return runtime, nil
	}, relay: func(ctx context.Context, _ relay.ClientConfig) error {
		*events = append(*events, "relay")
		if stop != nil {
			stop()
		}
		<-ctx.Done()
		return ctx.Err()
	}}
}

func TestBootstrapStageFetchesThenReexecesBeforeServices(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	// Тест проходит через реальный installArtifact: как TestInstallArtifact,
	// уводим artifactRoot во временный каталог, не в системный /opt/brood.
	base := t.TempDir()
	previous := artifactRoot
	artifactRoot = base
	t.Cleanup(func() { artifactRoot = previous })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	network := newFake()
	d := dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) {
		events = append(events, "network")
		return network, nil
	}, start: func(context.Context, []string) (actorRuntime, error) {
		events = append(events, "services")
		return newFake(), nil
	}, relay: func(context.Context, relay.ClientConfig) error {
		events = append(events, "relay")
		return nil
	}, artifacts: func(_ context.Context, cfg relay.ClientConfig, hosts []string, install func(context.Context, string, io.Reader, int64) error) error {
		events = append(events, "artifacts")
		if cfg.Token != "private-token" || !reflect.DeepEqual(hosts, []string{"github.com"}) {
			t.Error("artifacts fetch did not receive the supervisor relay config")
		}
		payload := "\x7fELFbootstrap"
		return install(context.Background(), "brood-guest", strings.NewReader(payload), int64(len(payload)))
	}, reexec: func(env []string) error {
		events = append(events, "reexec")
		// Настоящий exec не возвращается: всё, что bootstrap должен закрыть,
		// закрыто до него, а runtime получает исходный bootstrap env.
		if !network.closed {
			t.Error("DHCP client of the bootstrap stage survives exec")
		}
		runtime, err := parseConfig(env)
		if err != nil || runtime.stage != "runtime" || runtime.relay.Token != "private-token" {
			t.Errorf("runtime env does not parse as the runtime stage: %v %+v", err, runtime.stage)
		}
		stages := 0
		for _, entry := range env {
			if strings.HasPrefix(entry, "BROOD_GUEST_STAGE=") {
				stages++
			}
		}
		if stages != 1 {
			t.Errorf("runtime env has %d stage entries", stages)
		}
		return nil
	}}
	// main очищает окружение процесса сразу после разбора: runtime не может
	// брать токен из os.Environ (#155, прод: BROOD_GUEST_RELAY_TOKEN is required).
	if err := dispatch(ctx, nil, func() []string {
		env := stageEnv(t, "bootstrap")
		t.Setenv("BROOD_GUEST_RELAY_TOKEN", "")
		os.Unsetenv("BROOD_GUEST_RELAY_TOKEN")
		return env
	}, d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"network", "artifacts", "reexec"}) {
		t.Fatalf("events %v", events)
	}
	if strings.HasPrefix(os.Getenv("PATH"), "/opt/brood/bin:") {
		t.Fatal("bootstrap prepended the runtime PATH")
	}
}

func TestBootstrapFailureFailsClosed(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	for _, failure := range []string{"artifacts", "reexec"} {
		t.Run(failure, func(t *testing.T) {
			d := dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) {
				return newFake(), nil // сеть теперь стартует раньше артефактов (#155)
			}, artifacts: func(context.Context, relay.ClientConfig, []string, func(context.Context, string, io.Reader, int64) error) error {
				if failure != "artifacts" {
					return nil
				}
				return errors.New("private-token")
			}, reexec: func([]string) error {
				if failure == "reexec" {
					return errors.New("private-token")
				}
				return nil
			}}
			err := dispatch(context.Background(), nil, func() []string { return stageEnv(t, "bootstrap") }, d)
			if err == nil || !strings.HasPrefix(err.Error(), "guest bootstrap failed:") || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unsafe or missing bootstrap failure: %v", err)
			}
		})
	}
}

func TestRuntimeStageSkipsBootstrapAndPrependsPath(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	d := supervisorDeps(t, &events, cancel)
	d.artifacts = func(context.Context, relay.ClientConfig, []string, func(context.Context, string, io.Reader, int64) error) error {
		t.Fatal("runtime stage fetched artifacts")
		return nil
	}
	d.reexec = func([]string) error {
		t.Fatal("runtime stage reexeced")
		return nil
	}
	if err := dispatch(ctx, nil, func() []string { return stageEnv(t, "runtime") }, d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"network", "services", "relay"}) {
		t.Fatalf("events %v", events)
	}
	if got := os.Getenv("PATH"); got != "/opt/brood/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Fatalf("runtime PATH %q", got)
	}
}

func TestInstallArtifact(t *testing.T) {
	base := t.TempDir()
	previous := artifactRoot
	artifactRoot = base
	t.Cleanup(func() { artifactRoot = previous })
	install := func(name, content string, size int64) error {
		return installArtifact(context.Background(), name, strings.NewReader(content), size)
	}
	exact := func(name, content string) error { return install(name, content, int64(len(content))) }
	if err := exact("brood-guest", "\x7fELFbootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := exact("bro", "\x7fELFclient"); err != nil {
		t.Fatal(err)
	}
	if err := exact("claude", "\x7fELFclaude"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"brood-guest": "\x7fELFbootstrap", "bro": "\x7fELFclient", "claude": "\x7fELFclaude"} {
		data, err := os.ReadFile(filepath.Join(base, "bin", name))
		if err != nil || string(data) != want {
			t.Fatalf("%s content: %q %v", name, data, err)
		}
		info, err := os.Lstat(filepath.Join(base, "bin", name))
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("%s mode: %+v %v", name, info, err)
		}
	}
	if residue, _ := os.ReadDir(filepath.Join(base, "tmp")); len(residue) != 0 {
		t.Fatalf("staging residue: %v", residue)
	}
	if err := exact("brood-guest", "\x7fELFbootstrap"); err != nil {
		t.Fatalf("identical reinstall: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(base, "bin", "brood-guest")); string(data) != "\x7fELFbootstrap" {
		t.Fatal("reinstall changed the binary")
	}
	if err := exact("brood-guest", "\x7fELFother"); err == nil {
		t.Fatal("changed content accepted over an existing binary")
	}
	if err := install("bro", "short", 100); err == nil {
		t.Fatal("short stream accepted")
	}
	if err := os.Mkdir(filepath.Join(base, "bin", "sh"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := exact("sh", "x"); err == nil {
		t.Fatal("non-regular destination accepted")
	}
	if data, _ := os.ReadFile(filepath.Join(base, "bin", "brood-guest")); string(data) != "\x7fELFbootstrap" {
		t.Fatal("failed installs changed an existing binary")
	}
}
