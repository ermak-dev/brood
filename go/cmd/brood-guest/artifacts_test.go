//go:build linux

package main

// FEATURE: Bootstrap guest binaries from verified server artifacts (#155).
// SPEC: The root supervisor stage splits: bootstrap installs the three
// verified artifacts as a release under /opt/brood/releases, switches
// /opt/brood/bin to it (#184) and execs the runtime stage of that release
// before any network service; runtime prepends /opt/brood/bin to the helper
// PATH and never fetches. BROOD_GUEST_STAGE is process env only, never a PVE
// variable: unknown values are rejected by name. A stream whose sha256
// differs from the manifest never becomes visible (#265); a changed server
// set after a deploy is a new release, not a refusal (#184).
// IMPLEMENTATION: Stage field in config, artifacts/reexec dependencies with
// fakes here, the real release installer under a redirected artifact root,
// real guest.FetchArtifacts against an httptest TLS server as Brood.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/proxmox/guest"
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
	runtime := newFake()
	return dependencies{euid: func() int { return 0 }, start: func(context.Context) (actorRuntime, error) {
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
	// Тест проходит через настоящую установку набора: artifactRoot уводим во
	// временный каталог, не в системный /opt/brood.
	root := newRoot(t)
	previous := artifactRoot
	artifactRoot = root
	t.Cleanup(func() { artifactRoot = previous })
	set := newReleaseSet("bootstrap")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []string
	d := dependencies{euid: func() int { return 0 }, start: func(context.Context) (actorRuntime, error) {
		events = append(events, "services")
		return newFake(), nil
	}, relay: func(context.Context, relay.ClientConfig) error {
		events = append(events, "relay")
		return nil
	}, artifacts: func(_ context.Context, cfg relay.ClientConfig, release guest.ArtifactRelease) error {
		events = append(events, "artifacts")
		if cfg.Token != "private-token" {
			t.Error("artifacts fetch did not receive the supervisor relay config")
		}
		server := &fakeServer{set: set}
		return release(context.Background(), set.manifest(), server.download)
	}, reexec: func(executable string, env []string) error {
		events = append(events, "reexec")
		// Runtime-стадия: brood-guest проверенного набора, а bin уже
		// указывает на него: фиксированные пути видят тот же набор.
		if want := filepath.Join(root, "releases", set.id(t), "brood-guest"); executable != want {
			t.Errorf("runtime executable %q, want %q", executable, want)
		}
		assertActive(t, root, set)
		// Настоящий exec не возвращается: runtime получает исходный bootstrap env.
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
	if !reflect.DeepEqual(events, []string{"artifacts", "reexec"}) {
		t.Fatalf("events %v", events)
	}
	if strings.HasPrefix(os.Getenv("PATH"), "/opt/brood/bin:") {
		t.Fatal("bootstrap prepended the runtime PATH")
	}
}

func TestBootstrapFailureFailsClosed(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	previous := artifactRoot
	t.Cleanup(func() { artifactRoot = previous })
	for _, failure := range []string{"artifacts", "release", "reexec"} {
		t.Run(failure, func(t *testing.T) {
			artifactRoot = newRoot(t)
			set := newReleaseSet("closed")
			reexeced := false
			d := dependencies{euid: func() int { return 0 }, artifacts: func(_ context.Context, _ relay.ClientConfig, release guest.ArtifactRelease) error {
				switch failure {
				case "artifacts":
					return errors.New("private-token")
				case "release":
					// Набор с чужим digest: установка отказывает, runtime не стартует.
					// Тело той же длины, что claude-closed, и с секретом внутри.
					server := &fakeServer{set: set, serve: map[string]string{"claude": "\x7fELFprivate-token"}}
					return release(context.Background(), set.manifest(), server.download)
				}
				server := &fakeServer{set: set}
				return release(context.Background(), set.manifest(), server.download)
			}, reexec: func(string, []string) error {
				reexeced = true
				if failure == "reexec" {
					return errors.New("private-token")
				}
				return nil
			}}
			err := dispatch(context.Background(), nil, func() []string { return stageEnv(t, "bootstrap") }, d)
			if err == nil || !strings.HasPrefix(err.Error(), "guest bootstrap failed:") || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unsafe or missing bootstrap failure: %v", err)
			}
			if failure != "reexec" && reexeced {
				t.Fatal("runtime stage started without a verified release")
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
	d.artifacts = func(context.Context, relay.ClientConfig, guest.ArtifactRelease) error {
		t.Fatal("runtime stage fetched artifacts")
		return nil
	}
	d.reexec = func(string, []string) error {
		t.Fatal("runtime stage reexeced")
		return nil
	}
	if err := dispatch(ctx, nil, func() []string { return stageEnv(t, "runtime") }, d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"services", "relay"}) {
		t.Fatalf("events %v", events)
	}
	if got := os.Getenv("PATH"); got != "/opt/brood/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Fatalf("runtime PATH %q", got)
	}
}

// sha256Hex — digest манифеста для содержимого: строчный hex sha256.
func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// artifactDelivery: Brood на httptest TLS: манифест описывает files, тела
// отдаются из files, пока serve не подменяет тело артефакта.
type artifactDelivery struct {
	mu       sync.Mutex
	files    map[string][]byte
	serve    map[string][]byte
	requests []string
}

// deploy заменяет набор сервера, как выкладка BROOD_ARTIFACTS_DIR.
func (d *artifactDelivery) deploy(files map[string][]byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files = files
}

// took отдаёт имена запрошенных артефактов ("" означает манифест) и сбрасывает журнал.
func (d *artifactDelivery) took() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	requests := d.requests
	d.requests = nil
	return requests
}

func (d *artifactDelivery) override(name string, body []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if body == nil {
		delete(d.serve, name)
		return
	}
	d.serve[name] = body
}

func (d *artifactDelivery) start(t *testing.T) relay.ClientConfig {
	t.Helper()
	token := strings.Repeat("a", 43)
	prefix := "/api/proxmox/artifacts/actor-1/1"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		name, _ := strings.CutPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
		d.requests = append(d.requests, name)
		if r.URL.Path == prefix {
			if len(d.files) == 0 {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			entries := []string{}
			for _, name := range []string{"bro", "brood-guest", "claude"} {
				entries = append(entries, fmt.Sprintf(`{"name":%q,"size":%d,"sha256":%q}`, name, len(d.files[name]), sha256Hex(string(d.files[name]))))
			}
			_, _ = w.Write([]byte(`{"artifacts":[` + strings.Join(entries, ",") + `]}`))
			return
		}
		body, ok := d.serve[name]
		if !ok {
			body, ok = d.files[name]
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return relay.ClientConfig{URL: strings.Replace(server.URL, "https:", "wss:", 1), ActorUID: "actor-1", Generation: 1, Token: token, CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}
}

// releaseFrom: release-зависимость bootstrap над корнем root: записывает
// исполняемый файл runtime-стадии проверенного набора.
func releaseFrom(root string, executable *string) guest.ArtifactRelease {
	return func(ctx context.Context, manifest guest.ArtifactManifest, download guest.ArtifactDownload) error {
		path, err := installRelease(ctx, root, manifest, download, func(error) {})
		*executable = path
		return err
	}
}

func TestFetchArtifactsRefusesWrongDigestBeforePublish(t *testing.T) {
	root := newRoot(t)
	files := map[string][]byte{"bro": []byte("\x7fELFclient"), "brood-guest": []byte("\x7fELFbootstrap"), "claude": []byte("\x7fELFclaude")}
	delivery := &artifactDelivery{files: files, serve: map[string][]byte{}}
	cfg := delivery.start(t)
	// Подмена в пути: размер из манифеста и ELF-магия на месте, sha256 другой.
	delivery.override("bro", []byte("\x7fELFclienX"))
	var executable string
	if err := guest.FetchArtifacts(context.Background(), cfg, releaseFrom(root, &executable)); err == nil || executable != "" {
		t.Fatalf("artifact with a wrong sha256 accepted: %q %v", executable, err)
	}
	if published, _ := os.ReadDir(filepath.Join(root, "releases")); len(published) != 0 {
		t.Errorf("wrong digest published into releases: %v", published)
	}
	if _, err := os.Lstat(filepath.Join(root, "bin")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("wrong digest published bin: %v", err)
	}
	// Следующий старт получает верный артефакт и ставит весь набор.
	delivery.override("bro", nil)
	if err := guest.FetchArtifacts(context.Background(), cfg, releaseFrom(root, &executable)); err != nil {
		t.Fatalf("correct delivery after a refused one: %v", err)
	}
	for name, want := range files {
		if data, err := os.ReadFile(filepath.Join(root, "bin", name)); err != nil || string(data) != string(want) {
			t.Fatalf("%s content: %q %v", name, data, err)
		}
	}
}

// TestStopStartAfterDeploySwitchesRelease: сценарий #184 на настоящем
// FetchArtifacts: повторный старт без выкладки не качает ничего, старт после
// выкладки ставит новый набор рядом с прежним, откат сервера возвращает
// прежний без загрузки.
func TestStopStartAfterDeploySwitchesRelease(t *testing.T) {
	root := newRoot(t)
	setFiles := func(set releaseSet) map[string][]byte {
		files := map[string][]byte{}
		for name, body := range set {
			files[name] = []byte(body)
		}
		return files
	}
	a, b := newReleaseSet("a"), newReleaseSet("b")
	delivery := &artifactDelivery{files: setFiles(a), serve: map[string][]byte{}}
	cfg := delivery.start(t)
	boot := func(want releaseSet, requests []string) {
		t.Helper()
		var executable string
		if err := guest.FetchArtifacts(context.Background(), cfg, releaseFrom(root, &executable)); err != nil {
			t.Fatal(err)
		}
		if executable != filepath.Join(root, "releases", want.id(t), "brood-guest") {
			t.Fatalf("runtime executable %q", executable)
		}
		assertActive(t, root, want)
		if got := delivery.took(); !slices.Equal(got, requests) {
			t.Fatalf("requests %q, want %q", got, requests)
		}
	}
	full := []string{"", "bro", "brood-guest", "claude"}
	boot(a, full)
	boot(a, []string{""})
	delivery.deploy(setFiles(b))
	boot(b, full)
	if previous := linkTarget(t, filepath.Join(root, "previous")); previous != "releases/"+a.id(t) {
		t.Fatalf("previous %q", previous)
	}
	delivery.deploy(setFiles(a))
	boot(a, []string{""})
	// Сервер недоступен: старт отказывает, а не грузит установленный набор.
	delivery.deploy(map[string][]byte{})
	var executable string
	if err := guest.FetchArtifacts(context.Background(), cfg, releaseFrom(root, &executable)); err == nil || executable != "" {
		t.Fatalf("start without a verified server manifest: %q %v", executable, err)
	}
}
