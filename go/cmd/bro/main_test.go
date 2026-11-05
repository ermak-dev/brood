package main

// FEATURE: Run the real CLI with isolated streams and state.
// SPEC: Строгие аргументы, сохранение доверия CA, безопасный logout и автономная сборка.
// IMPLEMENTATION: runWithIO вызывает настоящие команды с переданными потоками.
// RESULT: IMPLEMENTED — контракт API, Git и Docker проверены без внешних сервисов.
// STATUS: DONE

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/clientapi"
)

func invoke(t *testing.T, input string, args ...string) (int, string, string) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	code := runWithIO(context.Background(), args, strings.NewReader(input), &out, &diagnostic)
	return code, out.String(), diagnostic.String()
}

func isolateState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("BUILD_REGISTRY", "")
	t.Setenv("BUILD_ATESPACE", "")
}

func TestCLIRejectsArguments(t *testing.T) {
	isolateState(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid arguments made a request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	defer server.Close()
	store, err := clientapi.NewSessionStore(filepath.Join(os.Getenv("XDG_STATE_HOME"), "brood", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrent(server.URL); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(server.URL, clientapi.StoredSession{Session: "session-secret", CSRF: "csrf-value"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"login", "https://one.example", "https://two.example"},
		{"logout", "--bad"}, {"whoami", "--bad"}, {"projects", "list", "--bad"},
		{"whoami", "https://one.example", "https://two.example"},
		{"login", "--ca="}, {"login", "--ca", "--bad"},
		{"build", "one", "two"}, {"build", "--unknown"},
		{"build", "--plan", "--export", "out"}, {"help", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, diagnostic := invoke(t, "", args...)
			if code == 0 || diagnostic == "" || out != "" {
				t.Fatalf("code=%d out=%q err=%q", code, out, diagnostic)
			}
		})
	}
}

func TestCLILoginTrustWhoamiProjectsAndLogout(t *testing.T) {
	isolateState(t)
	failLogout := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/session", func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie("session")
		http.SetCookie(w, &http.Cookie{Name: "csrf", Value: "csrf-value", Path: "/"})
		if c == nil {
			fmt.Fprint(w, `{"authenticated":false}`)
			return
		}
		fmt.Fprint(w, `{"authenticated":true,"user":{"username":"alice","display_name":"Alice","is_admin":false}}`)
	})
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var credentials map[string]string
		if err := json.NewDecoder(r.Body).Decode(&credentials); err != nil {
			t.Error(err)
		}
		c, err := r.Cookie("csrf")
		if err != nil || c.Value != r.Header.Get("X-CSRFToken") {
			t.Error("missing CSRF")
		}
		if credentials["username"] != "alice" || credentials["password"] != "password-secret" {
			t.Error("wrong credentials")
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "session-secret", Path: "/"})
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("session"); err != nil {
			t.Error("missing session")
		}
		fmt.Fprint(w, `[{"id":"p1","name":"Example"}]`)
	})
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-CSRFToken") != "csrf-value" {
			t.Error("missing logout CSRF")
		}
		if failLogout {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"detail":"session-secret"}`)
			return
		}
		w.WriteHeader(204)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, diagnostic := invoke(t, "alice\npassword-secret\n", "login", server.URL, "--ca", ca)
	if code != 0 || !strings.Contains(out, "logged in as alice") {
		t.Fatalf("%d %q %q", code, out, diagnostic)
	}
	// A later process must not need --ca or the original CA file.
	if err := os.Remove(ca); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"whoami"}, {"projects", "list"}} {
		code, out, diagnostic = invoke(t, "", args...)
		if code != 0 || out == "" {
			t.Fatalf("%v: %d %q %q", args, code, out, diagnostic)
		}
	}
	failLogout = true
	code, out, diagnostic = invoke(t, "", "logout")
	if code == 0 || strings.Contains(out, "logged out") || strings.Contains(diagnostic, "session-secret") {
		t.Fatalf("%d %q %q", code, out, diagnostic)
	}
	store, err := clientapi.NewSessionStore(filepath.Join(os.Getenv("XDG_STATE_HOME"), "brood", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.Load(server.URL)
	if err != nil || !ok || stored.Session != "session-secret" {
		t.Fatalf("lost session: %+v %v %v", stored, ok, err)
	}
	// The same credentials must also survive a transport failure.
	server.Close()
	code, out, diagnostic = invoke(t, "", "logout")
	if code == 0 || strings.Contains(out, "logged out") {
		t.Fatalf("%d %q %q", code, out, diagnostic)
	}
	stored, ok, err = store.Load(server.URL)
	if err != nil || !ok || stored.Session != "session-secret" {
		t.Fatalf("lost session after transport failure: %+v %v %v", stored, ok, err)
	}
}

func TestCLILogoutSuccess(t *testing.T) {
	isolateState(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/logout" || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	store, err := clientapi.NewSessionStore(filepath.Join(os.Getenv("XDG_STATE_HOME"), "brood", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(server.URL, clientapi.StoredSession{Session: "secret", CSRF: "csrf"}); err != nil {
		t.Fatal(err)
	}
	code, out, diagnostic := invoke(t, "", "logout", server.URL)
	if code != 0 || !strings.Contains(out, "logged out") {
		t.Fatalf("%d %q %q", code, out, diagnostic)
	}
	if _, ok, err := store.Load(server.URL); err != nil || ok {
		t.Fatalf("session not removed: %v %v", ok, err)
	}
}

func TestCLIAuthenticationFailures(t *testing.T) {
	isolateState(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"detail":"password-secret"}`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "csrf", Value: "csrf-value", Path: "/"})
		fmt.Fprint(w, `{"authenticated":false}`)
	}))
	defer server.Close()
	for _, args := range [][]string{{"login", server.URL}, {"whoami", server.URL}} {
		code, out, diagnostic := invoke(t, "alice\npassword-secret\n", args...)
		if code != 2 || diagnostic == "" || strings.Contains(out+diagnostic, "password-secret") || strings.Contains(out, "logged in") {
			t.Fatalf("%v: %d %q %q", args, code, out, diagnostic)
		}
	}
	store, err := clientapi.NewSessionStore(filepath.Join(os.Getenv("XDG_STATE_HOME"), "brood", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Load(server.URL); err != nil || ok {
		t.Fatalf("failed login saved state: %v %v", ok, err)
	}
}

func TestCLIRejectsRelativeState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative-state")
	code, _, diagnostic := invoke(t, "", "login", "https://example.test")
	if code == 0 || !strings.Contains(diagnostic, "absolute") {
		t.Fatalf("%d %q", code, diagnostic)
	}
}

func gitProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	data, err := os.ReadFile("../../brood/build/testdata/minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Broodfile"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "environment"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "environment", "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--quiet", "-m", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	return root
}

func fakeDocker(t *testing.T, body string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOCKER_CALLS\"\n" + body
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CALLS", log)
	return log
}

func TestCLIBuildPlanAndExportWithoutLogin(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	log := fakeDocker(t, "exit 99\n")
	// An invalid state root must not affect build-only commands.
	t.Setenv("XDG_STATE_HOME", "relative-state")
	t.Chdir(root)
	args := []string{"build", "development", "--project", "dev"}
	code, out, diagnostic := invoke(t, "", append(args, "--plan")...)
	var plan map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &plan) != nil || plan["atespace"] != "dev" {
		t.Fatalf("%d %q %q", code, out, diagnostic)
	}
	exported := filepath.Join(t.TempDir(), "context")
	code, out, diagnostic = invoke(t, "", append(args, "--export", exported)...)
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, diagnostic)
	}
	if _, err := os.Stat(filepath.Join(exported, "project", "environment", "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("offline modes invoked docker: %v", err)
	}
	t.Setenv("BUILD_PROJECT", "dev")
	code, out, diagnostic = invoke(t, "", "build", "--plan")
	if code != 0 {
		t.Fatalf("environment: %d %q %q", code, out, diagnostic)
	}
}

func TestCLIBuildPublishesSameBuildDigest(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	prepareBuildSync(t, root)
	archive, digest := cliOCIArchive(t)
	t.Setenv("FAKE_OCI_ARCHIVE", archive)
	t.Setenv("FAKE_OCI_DIGEST", digest)
	t.Chdir(root)
	log := fakeDocker(t, `[ "$1 $2" = "buildx build" ] || exit 90
while [ "$#" -gt 0 ]; do
 if [ "$1" = "--metadata-file" ]; then shift; metadata="$1"; fi
 if [ "$1" = "--output" ]; then shift; output="${1#type=oci,dest=}"; output="${output%%,*}"; fi
 shift
done
cp "$FAKE_OCI_ARCHIVE" "$output"
printf '{"containerimage.digest":"%s"}' "$FAKE_OCI_DIGEST" > "$metadata"
`)
	code, out, diagnostic := invoke(t, "", "build", "--project", "dev")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, diagnostic)
	}
	data, err := os.ReadFile(filepath.Join(root, ".brood", "build", "development", "build.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "brood.local/example/python@"+digest) {
		t.Fatalf("wrong digest: %s", data)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "\n") != 1 || strings.Contains(string(calls), "--push") || !strings.Contains(string(calls), "type=oci,dest=") {
		t.Fatalf("unexpected docker mutations: %s", calls)
	}
	if _, err := os.Stat(filepath.Join(root, ".brood", "build", "development", "image.oci.tar")); err != nil {
		t.Fatal(err)
	}
}

func TestCLIBuildFailureHasDiagnosticsAndNoArtifacts(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	prepareBuildSync(t, root)
	fakeDocker(t, "printf 'Dockerfile: invalid instruction\\n' >&2\nexit 42\n")
	t.Chdir(root)
	code, out, diagnostic := invoke(t, "", "build", "--project", "dev")
	if code == 0 || out != "" || !strings.Contains(diagnostic, "Dockerfile: invalid instruction") || !strings.Contains(diagnostic, "42") {
		t.Fatalf("%d %q %q", code, out, diagnostic)
	}
	if _, err := os.Lstat(filepath.Join(root, ".brood", "build", "development")); !os.IsNotExist(err) {
		t.Fatalf("failed build published output: %v", err)
	}
}

func TestCLIBuildDefaultsProjectWithoutRegistryOrLogin(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	t.Chdir(root)
	log := fakeDocker(t, "exit 90\n")
	code, out, diagnostic := invoke(t, "", "build", "--plan")
	var plan map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &plan) != nil || plan["atespace"] != "example" {
		t.Fatalf("%d %q %q", code, out, diagnostic)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("plan invoked docker: %v", err)
	}
}

func cliOCIArchive(t *testing.T) (string, string) {
	t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(config))
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`, configDigest, len(config)))
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d}]}`, digest, len(manifest)))
	path := filepath.Join(t.TempDir(), "image.oci.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for name, data := range map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`), "index.json": index,
		"blobs/sha256/" + strings.TrimPrefix(configDigest, "sha256:"): config,
		"blobs/sha256/" + strings.TrimPrefix(digest, "sha256:"):       manifest} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path, digest
}
