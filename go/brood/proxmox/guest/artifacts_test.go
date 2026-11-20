//go:build linux

package guest

// FEATURE: Bootstrap guest binaries from verified server artifacts (#155).
// SPEC: ParseArtifactManifest accepts exactly one object with one artifacts
// array of three entries (name/size/sha256, no unknown or duplicate keys,
// closed name list, 0 < size <= 512 MiB, lowercase hex64 digest, no trailing
// documents, 16 KiB read limit). FetchArtifacts GETs the manifest and hands
// it to the release before any artifact request (#184); the release downloads
// only the manifest artifacts it needs. Each download runs on the verified
// server route without proxy or redirects, checks the ELF magic, streams
// through sha256 into the installer together with the manifest size and
// digest and succeeds only when the installer consumed exactly size bytes and
// the digest matches. ReleaseID names the set by its canonical manifest text.
// IMPLEMENTATION: httptest TLS server as Brood, recording installer.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

const artifactTestToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// artifactBody строит правдоподобный бинарный артефакт с ELF-магией.
func artifactBody(seed string) []byte {
	return append([]byte{0x7f, 'E', 'L', 'F'}, seed...)
}

// artifactFiles — канонический набор из трёх артефактов.
func artifactFiles() map[string][]byte {
	return map[string][]byte{"bro": artifactBody("bro-payload"), "brood-guest": artifactBody("supervisor-payload"), "claude": artifactBody("claude-payload")}
}

// artifactManifestJSON собирает манифест из файлов в отсортированном порядке.
func artifactManifestJSON(files map[string][]byte) string {
	parts := []string{}
	for _, name := range []string{"bro", "brood-guest", "claude"} {
		data, ok := files[name]
		if !ok {
			continue
		}
		sum := sha256.Sum256(data)
		parts = append(parts, fmt.Sprintf(`{"name":%q,"size":%d,"sha256":%q}`, name, len(data), hex.EncodeToString(sum[:])))
	}
	return `{"artifacts":[` + strings.Join(parts, ",") + `]}`
}

type artifactServer struct {
	*httptest.Server
	mu       sync.Mutex
	manifest string
	status   int
	redirect string
	files    map[string][]byte
	serve    map[string][]byte
	requests []string
}

func newArtifactServer(t *testing.T, files map[string][]byte) *artifactServer {
	t.Helper()
	s := &artifactServer{status: http.StatusOK, files: files, serve: map[string][]byte{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.RawQuery != "" || len(r.Header.Values("Authorization")) != 1 || r.Header.Get("Authorization") != "Bearer "+artifactTestToken || r.Header.Get("Cookie") != "" || r.ContentLength > 0 {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		prefix := "/api/proxmox/artifacts/actor-1/1"
		if s.redirect != "" {
			http.Redirect(w, r, s.redirect, http.StatusFound)
			return
		}
		if r.URL.Path == prefix {
			s.requests = append(s.requests, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.status)
			_, _ = w.Write([]byte(s.manifest))
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, prefix+"/")
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.requests = append(s.requests, r.URL.Path)
		body, ok := s.serve[name]
		if !ok {
			body = s.files[name]
		}
		if body == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *artifactServer) config() relay.ClientConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return relay.ClientConfig{URL: strings.Replace(s.URL, "https:", "wss:", 1), ActorUID: "actor-1", Generation: 1, Token: artifactTestToken, CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})}
}

func (s *artifactServer) set(manifest string, status int) {
	s.mu.Lock()
	s.manifest, s.status = manifest, status
	s.mu.Unlock()
}

func (s *artifactServer) requestsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.requests...)
}

type artifactInstall struct {
	name   string
	size   int64
	digest string
	data   []byte
}

// recordInstall читает поток установки целиком, как настоящий установщик.
func recordInstall(records *[]artifactInstall, fail bool) ArtifactInstaller {
	return func(_ context.Context, name string, reader io.Reader, size int64, digest string) error {
		if fail {
			return fmt.Errorf("install refused %s", name)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		*records = append(*records, artifactInstall{name: name, size: size, digest: digest, data: data})
		return nil
	}
}

// installEach: релиз, который качает каждый артефакт манифеста через install
// в порядке манифеста, как прежний FetchArtifacts.
func installEach(install ArtifactInstaller) ArtifactRelease {
	return func(ctx context.Context, manifest ArtifactManifest, download ArtifactDownload) error {
		for _, artifact := range manifest.Artifacts {
			if err := download(ctx, artifact, install); err != nil {
				return err
			}
		}
		return nil
	}
}

// boundedArtifactError отвергает утечку токена и тел ответов в причине отказа.
func boundedArtifactError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("artifact failure accepted")
	}
	if text := err.Error(); len(text) > 128 || strings.Contains(text, artifactTestToken) || strings.Contains(text, "payload") || strings.ContainsAny(text, "\r\n\x1b") {
		t.Fatalf("unbounded artifact failure: %q", text)
	}
}

func TestParseArtifactManifest(t *testing.T) {
	files := artifactFiles()
	manifest, err := ParseArtifactManifest([]byte(artifactManifestJSON(files)))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, artifact := range manifest.Artifacts {
		names = append(names, artifact.Name)
		sum := sha256.Sum256(files[artifact.Name])
		if artifact.Size != int64(len(files[artifact.Name])) || artifact.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("artifact %+v does not describe %s", artifact, artifact.Name)
		}
	}
	if !slices.Equal(names, []string{"bro", "brood-guest", "claude"}) {
		t.Fatalf("names %v", names)
	}
	digest := strings.Repeat("0a", 32)
	entry := func(body string) string { return body }
	bad := map[string]string{
		"unknown field":       `{"artifacts":[{"name":"bro","size":10,"sha256":"` + digest + `","extra":1},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"duplicate entry key": `{"artifacts":[{"name":"bro","name":"bro","size":10,"sha256":"` + digest + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"duplicate top key":   `{"artifacts":[],"artifacts":[]}`,
		"missing name":        `{"artifacts":[{"name":"bro","size":10,"sha256":"` + digest + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"}]}`,
		"extra name":          `{"artifacts":[{"name":"bro","size":10,"sha256":"` + digest + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"},{"name":"sh","size":10,"sha256":"` + digest + `"}]}`,
		"unknown name":        `{"artifacts":[{"name":"bro","size":10,"sha256":"` + digest + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"sh","size":10,"sha256":"` + digest + `"}]}`,
		"zero size":           `{"artifacts":[{"name":"bro","size":0,"sha256":"` + digest + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"oversize":            `{"artifacts":[{"name":"bro","size":536870913,"sha256":"` + digest + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"negative size":       `{"artifacts":[{"name":"bro","size":-1,"sha256":"` + digest + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"fractional size":     `{"artifacts":[{"name":"bro","size":1.5,"sha256":"` + digest + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"short digest":        `{"artifacts":[{"name":"bro","size":10,"sha256":"0a"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"uppercase digest":    `{"artifacts":[{"name":"bro","size":10,"sha256":"` + strings.Repeat("0A", 32) + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"non-hex digest":      `{"artifacts":[{"name":"bro","size":10,"sha256":"` + strings.Repeat("gz", 32) + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"missing artifacts":   `{"origin":""}`,
		"null artifacts":      `{"artifacts":null}`,
		"empty artifacts":     `{"artifacts":[]}`,
		"artifacts not array": `{"artifacts":{}}`,
		"entry not object":    `{"artifacts":[1,2,3]}`,
		"missing entry field": `{"artifacts":[{"name":"bro","size":10},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"null entry field":    `{"artifacts":[{"name":null,"size":10,"sha256":"` + digest + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"wrong field type":    `{"artifacts":[{"name":"bro","size":"10","sha256":"` + digest + `"},{"name":"brood-guest","size":10,"sha256":"` + digest + `"},{"name":"claude","size":10,"sha256":"` + digest + `"}]}`,
		"trailing document":   artifactManifestJSON(files) + `{"extra":1}`,
		"array root":          `["private-token"]`,
		"not json":            entry(`private-token`),
		"empty body":          ``,
		"oversized manifest":  `{"artifacts":[` + strings.Repeat(" ", 1) + `]}` + strings.Repeat(" ", 16<<10),
		"padded manifest":     ` ` + artifactManifestJSON(files),
	}
	_ = entry
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			manifest, err := ParseArtifactManifest([]byte(body))
			if err == nil || len(manifest.Artifacts) != 0 || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("manifest accepted: %+v %v", manifest, err)
			}
		})
	}
}

func TestFetchArtifacts(t *testing.T) {
	files := artifactFiles()
	server := newArtifactServer(t, files)
	server.set(artifactManifestJSON(files), http.StatusOK)
	cfg := server.config()
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	var records []artifactInstall
	if err := FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&records, false))); err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("installs: %+v", records)
	}
	for i, want := range []string{"bro", "brood-guest", "claude"} {
		sum := sha256.Sum256(files[want])
		if records[i].name != want || records[i].size != int64(len(files[want])) || records[i].digest != hex.EncodeToString(sum[:]) || string(records[i].data) != string(files[want]) {
			t.Fatalf("install %d: %+v want %s", i, records[i], want)
		}
	}
	if got := server.requestsSnapshot(); !slices.Equal(got, []string{"/api/proxmox/artifacts/actor-1/1", "/api/proxmox/artifacts/actor-1/1/bro", "/api/proxmox/artifacts/actor-1/1/brood-guest", "/api/proxmox/artifacts/actor-1/1/claude"}) {
		t.Fatalf("requests %v", got)
	}
	// Повторная загрузка проходит: манифест и тела не менялись.
	records = nil
	if err := FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&records, false))); err != nil || len(records) != 3 {
		t.Fatalf("second fetch: %v %+v", err, records)
	}
}

func TestFetchArtifactsRejectsBadDelivery(t *testing.T) {
	files := artifactFiles()
	newFixture := func(t *testing.T) (*artifactServer, relay.ClientConfig) {
		server := newArtifactServer(t, files)
		server.set(artifactManifestJSON(files), http.StatusOK)
		return server, server.config()
	}
	t.Run("wrong digest", func(t *testing.T) {
		server, cfg := newFixture(t)
		server.mu.Lock()
		server.serve["bro"] = append(artifactBody("bro-payload"), 'X')
		server.mu.Unlock()
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, false))))
	})
	t.Run("not elf", func(t *testing.T) {
		server, cfg := newFixture(t)
		server.mu.Lock()
		server.serve["bro"] = []byte("#!/bin/sh\nprivate-token\n")
		server.mu.Unlock()
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, false))))
	})
	t.Run("truncated stream", func(t *testing.T) {
		server, cfg := newFixture(t)
		server.mu.Lock()
		server.serve["claude"] = files["claude"][:len(files["claude"])-2]
		server.mu.Unlock()
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, false))))
	})
	t.Run("stream longer than size", func(t *testing.T) {
		server, cfg := newFixture(t)
		server.mu.Lock()
		server.serve["bro"] = append(append([]byte{}, files["bro"]...), 'X')
		server.mu.Unlock()
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, false))))
	})
	t.Run("empty stream", func(t *testing.T) {
		server, cfg := newFixture(t)
		server.mu.Lock()
		server.serve["brood-guest"] = []byte{}
		server.mu.Unlock()
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, false))))
	})
	t.Run("install refused", func(t *testing.T) {
		_, cfg := newFixture(t)
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, true))))
	})
	t.Run("install did not consume", func(t *testing.T) {
		_, cfg := newFixture(t)
		skipping := func(context.Context, string, io.Reader, int64, string) error { return nil }
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(skipping)))
	})
	t.Run("artifact status", func(t *testing.T) {
		server, cfg := newFixture(t)
		server.mu.Lock()
		server.files["bro"] = nil
		server.mu.Unlock()
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, false))))
	})
	for name, manifest := range map[string]string{
		"manifest status":  "",
		"manifest invalid": `{"artifacts":[{` + strings.Repeat("\"x\",", 100),
	} {
		t.Run(name, func(t *testing.T) {
			server, cfg := newFixture(t)
			server.set(manifest, func() int {
				if manifest == "" {
					return http.StatusServiceUnavailable
				}
				return http.StatusOK
			}())
			boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, false))))
		})
	}
	t.Run("redirect denied", func(t *testing.T) {
		server, cfg := newFixture(t)
		redirected := false
		sink := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
		defer sink.Close()
		server.mu.Lock()
		server.redirect = sink.URL
		server.mu.Unlock()
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, false))))
		if redirected {
			t.Fatal("redirect followed")
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		_, cfg := newFixture(t)
		cfg.CAPEM = nil
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, false))))
	})
	t.Run("invalid route", func(t *testing.T) {
		_, cfg := newFixture(t)
		cfg.Token = "short"
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, installEach(recordInstall(&[]artifactInstall{}, false))))
	})
}

// TestArtifactManifestReleaseID: id набора (#184) есть sha256 канонического
// текста манифеста: не зависит от порядка записей, меняется с любым именем,
// размером или digest; у манифеста вне закрытого списка id нет.
func TestArtifactManifestReleaseID(t *testing.T) {
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	manifest := ArtifactManifest{Artifacts: []Artifact{{"bro", 10, a}, {"brood-guest", 20, b}, {"claude", 30, c}}}
	// Независимый расчёт: printf 'brood guest release v1\nbro 10 a…\n
	// brood-guest 20 b…\nclaude 30 c…\n' | sha256sum.
	const want = "f9e92918e7eee8ea2afc3e83010634025f668241533ce140af9a3a7a662dd12f"
	if id, err := manifest.ReleaseID(); err != nil || id != want {
		t.Fatalf("release id %q %v, want %s", id, err, want)
	}
	reordered := ArtifactManifest{Artifacts: []Artifact{manifest.Artifacts[2], manifest.Artifacts[0], manifest.Artifacts[1]}}
	if id, err := reordered.ReleaseID(); err != nil || id != want {
		t.Fatalf("reordered manifest id %q %v", id, err)
	}
	with := func(i int, change func(*Artifact)) ArtifactManifest {
		m := ArtifactManifest{Artifacts: append([]Artifact{}, manifest.Artifacts...)}
		change(&m.Artifacts[i])
		return m
	}
	changed := map[string]ArtifactManifest{
		"size":    with(2, func(x *Artifact) { x.Size = 31 }),
		"digest":  with(0, func(x *Artifact) { x.SHA256 = strings.Repeat("d", 64) }),
		"swapped": {Artifacts: []Artifact{{"bro", 10, b}, {"brood-guest", 20, a}, {"claude", 30, c}}},
	}
	seen := map[string]string{want: "original"}
	for name, m := range changed {
		id, err := m.ReleaseID()
		if err != nil || len(id) != 64 || !isLowerHex(id) {
			t.Fatalf("%s: id %q %v", name, id, err)
		}
		if other, dup := seen[id]; dup {
			t.Fatalf("%s: id equals %s", name, other)
		}
		seen[id] = name
	}
	invalid := map[string]ArtifactManifest{
		"empty":            {},
		"missing name":     {Artifacts: manifest.Artifacts[:2]},
		"duplicate name":   {Artifacts: []Artifact{{"bro", 10, a}, {"bro", 10, a}, {"claude", 30, c}}},
		"unknown name":     {Artifacts: []Artifact{{"bro", 10, a}, {"sh", 20, b}, {"claude", 30, c}}},
		"extra entry":      {Artifacts: append(append([]Artifact{}, manifest.Artifacts...), Artifact{"bro", 10, a})},
		"zero size":        with(1, func(x *Artifact) { x.Size = 0 }),
		"oversize":         with(1, func(x *Artifact) { x.Size = 512<<20 + 1 }),
		"uppercase digest": with(1, func(x *Artifact) { x.SHA256 = strings.Repeat("B", 64) }),
		"short digest":     with(1, func(x *Artifact) { x.SHA256 = "bb" }),
		"newline in name":  with(0, func(x *Artifact) { x.Name = "bro\nclaude 1 " + c }),
	}
	for name, m := range invalid {
		if id, err := m.ReleaseID(); err == nil || id != "" {
			t.Fatalf("%s: invalid manifest got id %q", name, id)
		}
	}
}

// TestFetchArtifactsHandsManifestToRelease: релиз получает разобранный
// манифест до первого запроса артефакта и качает только то, что ему нужно;
// чужой артефакт загрузчик не запрашивает (#184).
func TestFetchArtifactsHandsManifestToRelease(t *testing.T) {
	files := artifactFiles()
	server := newArtifactServer(t, files)
	server.set(artifactManifestJSON(files), http.StatusOK)
	cfg := server.config()
	want, err := ParseArtifactManifest([]byte(artifactManifestJSON(files)))
	if err != nil {
		t.Fatal(err)
	}
	manifestOnly := []string{"/api/proxmox/artifacts/actor-1/1"}
	// Проверенный установленный набор: релиз не качает ничего.
	reuse := func(_ context.Context, manifest ArtifactManifest, _ ArtifactDownload) error {
		if !slices.Equal(manifest.Artifacts, want.Artifacts) {
			t.Errorf("release manifest %+v, want %+v", manifest, want)
		}
		if got := server.requestsSnapshot(); !slices.Equal(got, manifestOnly) {
			t.Errorf("artifacts requested before the release decided: %v", got)
		}
		return nil
	}
	if err := FetchArtifacts(context.Background(), cfg, reuse); err != nil {
		t.Fatal(err)
	}
	if got := server.requestsSnapshot(); !slices.Equal(got, manifestOnly) {
		t.Fatalf("reuse downloaded artifacts: %v", got)
	}
	// Загрузчик принимает только записи манифеста: чужое имя или чужой digest
	// не уходят на сервер.
	for name, foreign := range map[string]Artifact{
		"other digest": {Name: "bro", Size: want.Artifacts[0].Size, SHA256: strings.Repeat("0", 64)},
		"other size":   {Name: "bro", Size: want.Artifacts[0].Size + 1, SHA256: want.Artifacts[0].SHA256},
		"other name":   {Name: "sh", Size: want.Artifacts[0].Size, SHA256: want.Artifacts[0].SHA256},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(server.requestsSnapshot())
			release := func(ctx context.Context, _ ArtifactManifest, download ArtifactDownload) error {
				return download(ctx, foreign, recordInstall(&[]artifactInstall{}, false))
			}
			boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, release))
			if got := server.requestsSnapshot()[before:]; !slices.Equal(got, manifestOnly) {
				t.Fatalf("foreign artifact requested: %v", got)
			}
		})
	}
	t.Run("release refused", func(t *testing.T) {
		refuse := func(context.Context, ArtifactManifest, ArtifactDownload) error {
			return fmt.Errorf("private-token payload")
		}
		boundedArtifactError(t, FetchArtifacts(context.Background(), cfg, refuse))
	})
}
