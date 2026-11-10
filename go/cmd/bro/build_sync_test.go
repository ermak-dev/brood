package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/clientapi"
)

// FEATURE: Обычная сборка синхронизирует декларацию проекта (#172).
// SPEC: Авторизация и синхронизация предшествуют Docker; отказ не публикует
// артефакты. Plan/export остаются офлайн; повтор передаёт ту же декларацию.
func saveBuildSession(t *testing.T, origin string) {
	t.Helper()
	store, err := clientapi.NewSessionStore(filepath.Join(os.Getenv("XDG_STATE_HOME"), "brood", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(origin, clientapi.StoredSession{Session: "fixture-session", CSRF: "fixture-csrf"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrent(origin); err != nil {
		t.Fatal(err)
	}
}

func addBuildOrigin(t *testing.T, root string) {
	t.Helper()
	cmd := exec.Command("git", "remote", "add", "origin", "git@git.example.com:team/current.git")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture origin: %v %s", err, output)
	}
}

func prepareBuildSync(t *testing.T, root string) {
	t.Helper()
	addBuildOrigin(t, root)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/projects/sync" {
			t.Errorf("unexpected build request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"project-id","name":"dev","repository":"https://git.example.com/team/current.git","created":false,"changed":false,"origin_mode":"gateway"}`))
	}))
	t.Cleanup(server.Close)
	saveBuildSession(t, server.URL)
}

func TestBuildRequiresProjectSyncBeforeDocker(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildOrigin(t, root)
	t.Chdir(root)
	calls := fakeDocker(t, "exit 99\n")
	code, _, diagnostic := invoke(t, "", "build")
	if code == 0 || !strings.Contains(diagnostic, "bro login") {
		t.Fatalf("build without login: %d %q", code, diagnostic)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("build ran Docker before project synchronization")
	}
}

func TestBuildSyncsSameDeclarationBeforeDocker(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildOrigin(t, root)
	t.Chdir(root)
	var declarations []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/projects/sync" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != "fixture-session" || r.Header.Get("X-CSRFToken") != "fixture-csrf" {
			t.Error("project synchronization omitted session or CSRF")
		}
		var declaration map[string]any
		if err := json.NewDecoder(r.Body).Decode(&declaration); err != nil {
			t.Error(err)
		}
		declarations = append(declarations, declaration)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"project-id","name":"example","repository":"https://git.example.com/team/current.git","created":false,"changed":false,"origin_mode":""}`))
	}))
	defer server.Close()
	saveBuildSession(t, server.URL)
	calls := fakeDocker(t, "printf 'fixture image build failed\\n' >&2\nexit 42\n")
	for i := 0; i < 2; i++ {
		code, _, diagnostic := invoke(t, "", "build")
		if code == 0 || !strings.Contains(diagnostic, "fixture image build failed") {
			t.Fatalf("fixture build: %d %q", code, diagnostic)
		}
		if len(declarations) != i+1 {
			t.Fatalf("project sync requests = %d, want %d", len(declarations), i+1)
		}
		if !strings.Contains(diagnostic, "no Git access mode") {
			t.Fatal("unusable clone access was not reported separately from repository declaration")
		}
	}
	if declarations[0]["name"] != "example" || declarations[0]["repository"] != "https://git.example.com/team/current.git" || !reflect.DeepEqual(declarations[0], declarations[1]) {
		t.Fatalf("project declarations = %v", declarations)
	}
	if _, err := os.Stat(calls); err != nil {
		t.Fatal("Docker did not run after successful project synchronization")
	}
}

func TestGuestBuildNeverUsesOperatorSession(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildOrigin(t, root)
	t.Chdir(root)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(500)
	}))
	defer server.Close()
	saveBuildSession(t, server.URL)
	calls := fakeDocker(t, "exit 99\n")
	code, _, diagnostic := invoke(t, "", "--guest", "build")
	if code == 0 || !strings.Contains(diagnostic, "operator mode") || requests != 0 {
		t.Fatalf("guest build: %d requests=%d %q", code, requests, diagnostic)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("guest build ran Docker without scoped synchronization")
	}
}

func TestBuildSyncFailureStopsBeforeDocker(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildOrigin(t, root)
	t.Chdir(root)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"project synchronization denied"}`))
	}))
	defer server.Close()
	saveBuildSession(t, server.URL)
	calls := fakeDocker(t, "exit 99\n")
	code, _, diagnostic := invoke(t, "", "build")
	if code == 0 || requests != 1 || !strings.Contains(diagnostic, "status 403") {
		t.Fatalf("sync refusal: %d requests=%d %q", code, requests, diagnostic)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("project sync refusal ran Docker")
	}
	if _, err := os.Lstat(filepath.Join(root, ".brood", "build")); !os.IsNotExist(err) {
		t.Fatal("project sync refusal published build artifacts")
	}
}

// FEATURE: bro build sends the Git credential reference and reports its state (#173).
// SPEC: project.git_credential travels in the sync declaration as a name;
// an unbound reference is reported by name with the command that fixes it,
// never with any value. A bound reference with gateway mode prints nothing.
func TestBuildSyncsGitCredentialReference(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildOrigin(t, root)
	file := filepath.Join(root, "Broodfile")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(data, []byte("project:\n  git_credential: gitlab\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	response := `{"id":"project-id","name":"example","repository":"https://git.example.com/team/current.git","created":false,"changed":false,"origin_mode":"","git_credential":"gitlab","git_credential_bound":false}`
	var declarations []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var declaration map[string]any
		if err := json.NewDecoder(r.Body).Decode(&declaration); err != nil {
			t.Error(err)
		}
		declarations = append(declarations, declaration)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()
	saveBuildSession(t, server.URL)
	fakeDocker(t, "exit 42\n")
	_, _, diagnostic := invoke(t, "", "build")
	if len(declarations) != 1 || declarations[0]["git_credential"] != "gitlab" || declarations[0]["repository"] != "https://git.example.com/team/current.git" {
		t.Fatalf("declaration = %v", declarations)
	}
	if !strings.Contains(diagnostic, "Git credential gitlab") || !strings.Contains(diagnostic, "bro secret --project example git add gitlab") {
		t.Fatalf("unbound reference not reported: %q", diagnostic)
	}
	response = `{"id":"project-id","name":"example","repository":"https://git.example.com/team/current.git","created":false,"changed":false,"origin_mode":"gateway","git_credential":"gitlab","git_credential_bound":true}`
	_, _, diagnostic = invoke(t, "", "build")
	if strings.Contains(diagnostic, "Git credential") || strings.Contains(diagnostic, "no Git access mode") {
		t.Fatalf("bound reference reported: %q", diagnostic)
	}
}
