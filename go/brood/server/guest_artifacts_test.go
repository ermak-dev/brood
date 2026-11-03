// Проверки доставки артефактов бутстрапа гостя (#155): манифест
// и поток файла из BROOD_ARTIFACTS_DIR с авторизацией гостя.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

// guestArtifactContents — содержимое трёх артефактов фикстуры: маленькие
// файлы, магию ELF сервер не проверяет (её проверяет гость).
var guestArtifactContents = map[string]string{
	"bro":         "bro\n",
	"brood-guest": "guest\n",
	"claude":      "claude\n",
}

// guestArtifactOrder — детерминированный порядок манифеста: по имени.
var guestArtifactOrder = []string{"bro", "brood-guest", "claude"}

func guestArtifactDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// wantGuestArtifactsJSON — точные байты ожидаемого манифеста фикстуры.
func wantGuestArtifactsJSON() string {
	entries := make([]string, 0, len(guestArtifactOrder))
	for _, name := range guestArtifactOrder {
		content := guestArtifactContents[name]
		entries = append(entries, fmt.Sprintf(`{"name":%q,"size":%d,"sha256":%q}`, name, len(content), guestArtifactDigest(content)))
	}
	return `{"artifacts":[` + strings.Join(entries, ",") + `]}`
}

// writeGuestArtifacts — временный каталог артефактов с manifest.json
// и файлами фикстуры.
func writeGuestArtifacts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range guestArtifactContents {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(wantGuestArtifactsJSON()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// artifactsFixture — запущенный gateway-актор фикстуры workspace и каталог
// артефактов в настройке сервера.
type artifactsFixture struct {
	workspaceFixture
	dir string
}

func artifactsSetup(t *testing.T) artifactsFixture {
	t.Helper()
	f := workspaceSetup(t)
	dir := writeGuestArtifacts(t)
	f.ts.srv.artifactsDir = dir
	return artifactsFixture{workspaceFixture: f, dir: dir}
}

func artifactsPath(a store.ManagedActor, name string) string {
	p := fmt.Sprintf("/api/proxmox/artifacts/%s/%d", a.UID, a.Incarnation)
	if name != "" {
		p += "/" + name
	}
	return p
}

// expectArtifacts требует статус с заголовками маршрута и точным телом
// ошибки writeDetail; detail пустой — тело не проверяется (поток файла).
func expectArtifacts(t *testing.T, what string, w *httptest.ResponseRecorder, status int, detail string) {
	t.Helper()
	if w.Code != status || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("%s: status=%d want=%d header=%v body=%s", what, w.Code, status, w.Header(), w.Body.String())
	}
	if detail != "" && w.Body.String() != "{\"detail\":\""+detail+"\"}\n" {
		t.Fatalf("%s: body %s; want detail %q", what, w.Body.String(), detail)
	}
}

// FEATURE: Guest bootstrap artifact delivery (#155).
// SPEC: GET /api/proxmox/artifacts/{uid}/{generation} returns the manifest of
// BROOD_ARTIFACTS_DIR as deterministic JSON sorted by name with exactly the
// closed name list, sizes within (0, 512MiB] and lowercase hex64 digests;
// the manifest file is reread on every request. Any missing setting, missing
// or malformed manifest or file mismatch answers 503 without filesystem
// details.
// IMPLEMENTATION: readGuestArtifacts validates manifest.json and the named
// regular files on each request; failures collapse to one static reason.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGuestArtifactsManifestDelivery(t *testing.T) {
	f := artifactsSetup(t)
	clone := artifactsPath(f.clone, "")
	ok := bearer(f.token(f.clone))

	w := f.serve("GET", clone, nil, ok)
	expectArtifacts(t, "manifest", w, http.StatusOK, "")
	if ct, want := w.Header().Get("Content-Type"), "application/json"; ct != want {
		t.Fatalf("content-type = %q; want %q", ct, want)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("nosniff missing: %v", w.Header())
	}
	if body := w.Body.String(); body != wantGuestArtifactsJSON() {
		t.Fatalf("manifest body %s; want %s", body, wantGuestArtifactsJSON())
	}

	// Манифест перечитывается на каждый запрос: новый digest виден сразу.
	changed := strings.Replace(wantGuestArtifactsJSON(), guestArtifactDigest("claude\n"), strings.Repeat("b", 64), 1)
	if err := os.WriteFile(filepath.Join(f.dir, "manifest.json"), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	w = f.serve("GET", clone, nil, ok)
	expectArtifacts(t, "reread", w, http.StatusOK, "")
	if w.Body.String() != changed {
		t.Fatalf("manifest not reread: %s; want %s", w.Body.String(), changed)
	}

	// Отказы — одна статичная причина, без деталей файловой системы.
	unavailable := "guest artifact delivery unavailable"
	for _, tc := range []struct {
		name   string
		mutate func(dir string)
	}{
		{"setting empty", func(string) { f.ts.srv.artifactsDir = "" }},
		{"dir missing", func(string) { f.ts.srv.artifactsDir = filepath.Join(f.dir, "gone") }},
		{"manifest missing", func(dir string) { os.Remove(filepath.Join(dir, "manifest.json")) }},
		{"manifest malformed", func(dir string) { os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("not json"), 0o644) }},
		{"manifest too large", func(dir string) {
			os.WriteFile(filepath.Join(dir, "manifest.json"), append([]byte(strings.Repeat(" ", 17000)), []byte(wantGuestArtifactsJSON())...), 0o644)
		}},
		{"two entries", func(dir string) {
			entries := fmt.Sprintf(`{"name":"bro","size":4,"sha256":%q},{"name":"claude","size":7,"sha256":%q}`, guestArtifactDigest("bro\n"), guestArtifactDigest("claude\n"))
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"artifacts":[`+entries+`]}`), 0o644)
		}},
		{"duplicate name", func(dir string) {
			entry := fmt.Sprintf(`{"name":"bro","size":4,"sha256":%q}`, guestArtifactDigest("bro\n"))
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"artifacts":[`+entry+`,`+entry+`,{"name":"claude","size":7,"sha256":"`+strings.Repeat("a", 64)+`"}]}`), 0o644)
		}},
		{"unknown name in manifest", func(dir string) {
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"artifacts":[{"name":"bro","size":4,"sha256":"`+strings.Repeat("a", 64)+`"},{"name":"other","size":1,"sha256":"`+strings.Repeat("b", 64)+`"},{"name":"claude","size":7,"sha256":"`+strings.Repeat("c", 64)+`"}]}`), 0o644)
		}},
		{"zero size", func(dir string) {
			manifest := strings.Replace(wantGuestArtifactsJSON(), `"name":"bro","size":4`, `"name":"bro","size":0`, 1)
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644)
		}},
		{"oversize entry", func(dir string) {
			manifest := strings.Replace(wantGuestArtifactsJSON(), `"name":"bro","size":4`, `"name":"bro","size":513414029`, 1)
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644)
		}},
		{"uppercase digest", func(dir string) {
			manifest := strings.Replace(wantGuestArtifactsJSON(), guestArtifactDigest("bro\n"), strings.ToUpper(strings.Repeat("a", 64)), 1)
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644)
		}},
		{"short digest", func(dir string) {
			manifest := strings.Replace(wantGuestArtifactsJSON(), guestArtifactDigest("bro\n"), strings.Repeat("a", 63), 1)
			os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644)
		}},
		{"file missing", func(dir string) { os.Remove(filepath.Join(dir, "bro")) }},
		{"size mismatch", func(dir string) {
			os.WriteFile(filepath.Join(dir, "bro"), []byte("br"), 0o755)
		}},
		{"file is directory", func(dir string) {
			os.Remove(filepath.Join(dir, "bro"))
			os.Mkdir(filepath.Join(dir, "bro"), 0o755)
		}},
	} {
		if tc.mutate != nil {
			tc.mutate(f.dir)
		}
		expectArtifacts(t, tc.name, f.serve("GET", clone, nil, ok), http.StatusServiceUnavailable, unavailable)
	}
}

// SPEC: Authorization is exactly that of /api/proxmox/secrets: one Bearer
// guest token for the UID, generation and current start intent, an active
// swarm binding; cookies never authorize. Unknown artifact names and
// malformed paths answer 404, non-GET methods 405.
// IMPLEMENTATION: The shared guest runtime authorization of the secrets
// route, applied to both the manifest and the file path.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGuestArtifactsAuthorization(t *testing.T) {
	f := artifactsSetup(t)
	manifest := artifactsPath(f.clone, "")
	file := artifactsPath(f.clone, "bro")
	token := f.token(f.clone)
	next := f.clone.Incarnation + 1
	nextToken := guestToken(f.ts.srv.managed.registry.relaySecret, f.clone.UID, next)
	for _, tc := range []struct {
		name, path string
		header     http.Header
	}{
		{"no token", manifest, nil},
		{"no token file", file, nil},
		{"empty bearer", manifest, http.Header{"Authorization": {"Bearer "}}},
		{"wrong token", manifest, bearer(strings.Repeat("A", 43))},
		{"wrong token file", file, bearer(strings.Repeat("A", 43))},
		{"other actor token", manifest, bearer(f.token(f.direct))},
		{"other generation token", manifest, bearer(nextToken)},
		{"cookie only", manifest, http.Header{"Cookie": {"session=x"}}},
	} {
		expectArtifacts(t, tc.name, f.serve("GET", tc.path, nil, tc.header), http.StatusUnauthorized, "guest authorization denied")
	}
	ok := bearer(token)
	for _, tc := range []struct {
		method, path string
		want         int
		detail       string
	}{
		{"POST", manifest, 405, "method not allowed"},
		{"POST", file, 405, "method not allowed"},
		{"PUT", file, 405, "method not allowed"},
		{"GET", manifest + "?x=1", 404, "not found"},
		{"GET", file + "?x=1", 404, "not found"},
		{"GET", artifactsPath(f.clone, "unknown"), 404, "not found"},
		{"GET", artifactsPath(f.clone, "manifest.json"), 404, "not found"},
		{"GET", file + "/extra", 404, "not found"},
		{"GET", "/api/proxmox/artifacts/" + f.clone.UID, 404, "not found"},
		{"GET", "/api/proxmox/artifacts/", 404, "not found"},
		{"GET", fmt.Sprintf("/api/proxmox/artifacts/%s/0%d", f.clone.UID, f.clone.Incarnation), 404, "not found"},
		{"GET", fmt.Sprintf("/api/proxmox/artifacts/%s/x", f.clone.UID), 404, "not found"},
		{"GET", "/api/proxmox/artifacts/bad.uid/1", 404, "not found"},
	} {
		w := f.serve(tc.method, tc.path, nil, ok)
		expectArtifacts(t, tc.method+" "+tc.path, w, tc.want, tc.detail)
		if tc.want == 405 && w.Header().Get("Allow") != http.MethodGet {
			t.Fatalf("%s: Allow=%q", tc.method, w.Header().Get("Allow"))
		}
	}
}

// SPEC: GET /api/proxmox/artifacts/{uid}/{generation}/{name} streams the
// named artifact as application/octet-stream with Content-Length and
// no-store; unknown names answer 404, missing or mismatched artifacts 503.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGuestArtifactsStreaming(t *testing.T) {
	f := artifactsSetup(t)
	ok := bearer(f.token(f.clone))
	for _, name := range guestArtifactOrder {
		w := f.serve("GET", artifactsPath(f.clone, name), nil, ok)
		content := guestArtifactContents[name]
		expectArtifacts(t, "stream "+name, w, http.StatusOK, "")
		if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Fatalf("%s content-type = %q", name, ct)
		}
		if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(len(content)) {
			t.Fatalf("%s content-length = %q; want %d", name, cl, len(content))
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s nosniff missing: %v", name, w.Header())
		}
		if w.Body.String() != content {
			t.Fatalf("%s bytes %q; want %q", name, w.Body.String(), content)
		}
	}

	// Настоящий клиент через слушатель сервера: те же байты и заголовки.
	req, err := http.NewRequest("GET", f.ts.http.URL+artifactsPath(f.clone, "claude"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token(f.clone))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(raw) != guestArtifactContents["claude"] ||
		response.Header.Get("Content-Type") != "application/octet-stream" || response.Header.Get("Content-Length") != strconv.Itoa(len(guestArtifactContents["claude"])) ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("listener fetch: %d %q %v", response.StatusCode, raw, err)
	}

	unavailable := "guest artifact delivery unavailable"
	expectArtifacts(t, "unknown name", f.serve("GET", artifactsPath(f.clone, "unknown"), nil, ok), http.StatusNotFound, "not found")
	expectArtifacts(t, "missing file", func() *httptest.ResponseRecorder {
		os.Remove(filepath.Join(f.dir, "bro"))
		return f.serve("GET", artifactsPath(f.clone, "bro"), nil, ok)
	}(), http.StatusServiceUnavailable, unavailable)
	f.ts.srv.artifactsDir = ""
	expectArtifacts(t, "unconfigured", f.serve("GET", artifactsPath(f.clone, "bro"), nil, ok), http.StatusServiceUnavailable, unavailable)
}

// SPEC: BROOD_ARTIFACTS_DIR is an optional machine setting: empty leaves
// delivery unconfigured; a nonempty value must be an absolute path with a
// nonempty basename, otherwise LoadConfig refuses naming the setting.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGuestArtifactsConfig(t *testing.T) {
	cfg, err := LoadConfig(func(key string) string {
		if key == "BROOD_ARTIFACTS_DIR" {
			return "'/srv/brood/artifacts'"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.artifactsDir != "/srv/brood/artifacts" {
		t.Fatalf("artifactsDir = %q", cfg.artifactsDir)
	}
	cfg, err = LoadConfig(func(string) string { return "" })
	if err != nil || cfg.artifactsDir != "" {
		t.Fatalf("empty env: %q %v", cfg.artifactsDir, err)
	}
	for _, value := range []string{"srv/artifacts", "artifacts", "/srv/brood/artifacts/", "/"} {
		_, err := LoadConfig(func(key string) string {
			if key == "BROOD_ARTIFACTS_DIR" {
				return value
			}
			return ""
		})
		var cfgErr *ConfigError
		if err == nil || !errors.As(err, &cfgErr) || cfgErr.Setting != "BROOD_ARTIFACTS_DIR" {
			t.Fatalf("value %q: refused without the setting name: %v", value, err)
		}
	}
}
