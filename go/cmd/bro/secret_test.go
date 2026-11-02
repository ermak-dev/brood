package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"brood.dev/go/brood/clientapi"
	"brood.dev/go/brood/projectsecret"
)

// FEATURE: Explicit-project, operator-only write-only secrets CLI.
// SPEC: No argv values, all files validated before PUT, bounded root-confined reads.
// IMPLEMENTATION: Explicit parsing, shared validators, rooted bounded reads and normal session client.
// RESULT: IMPLEMENTED — CLI and loopback transport tests pass, including race checks.
// STATUS: DONE
func secretServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	isolateState(t)
	t.Setenv("BROOD_EXECUTION_MODE", "")
	t.Setenv("BROOD_SWARM", "")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	store, err := clientapi.NewSessionStore(filepath.Join(os.Getenv("XDG_STATE_HOME"), "brood", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetCurrent(server.URL); err != nil {
		t.Fatal(err)
	}
	if err = store.Save(server.URL, clientapi.StoredSession{Session: "s", CSRF: "c"}); err != nil {
		t.Fatal(err)
	}
}

func TestSecretCLI(t *testing.T) {
	puts := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/projects":
			fmt.Fprint(w, `[{"id":"p1","name":"demo"},{"id":"other","name":"p1"}]`)
		case "/api/projects/p1/secrets/vars":
			if r.Method != "PUT" {
				t.Error("not PUT")
			}
			puts++
			var body struct {
				Vars map[string]string `json:"vars"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Vars["TOKEN"] != "dummy" {
				t.Error("stdin newline handling")
			}
			fmt.Fprint(w, `{"revision":1,"vars":["TOKEN"]}`)
		case "/api/projects/p1/secrets":
			fmt.Fprint(w, `{"revision":1,"files":[{"name":"config/key","size":5}],"vars":["TOKEN"]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	for _, project := range []string{"demo", "p1"} {
		code, out, diag := invoke(t, "dummy\n", "secret", "--project", project, "var", "add", "TOKEN", "--stdin")
		if code != 0 || out != "" || diag != "" {
			t.Fatalf("add %d %q %q", code, out, diag)
		}
	}
	code, out, diag := invoke(t, "", "secret", "--project", "demo", "file", "list")
	if code != 0 || out != "config/key\t5\n" || diag != "" {
		t.Fatalf("file list %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "secret", "--project", "p1", "var", "list")
	if code != 0 || out != "TOKEN\n" || diag != "" || puts != 2 {
		t.Fatalf("var list %d %q %q", code, out, diag)
	}
}

func TestSecretCLIRejectsUnsafeInput(t *testing.T) {
	calls := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, `[]`) })
	cases := [][]string{
		{"secret", "var", "add", "TOKEN", "--stdin"},
		{"secret", "--project", "p", "var", "add", "TOKEN", "dummy-sensitive-value"},
		{"secret", "--project", "p", "var", "add", "TOKEN", "--value", "dummy-sensitive-value"},
		{"secret", "--project", "p", "var", "add", "dummy-sensitive-value", "--stdin"},
		{"secret", "--project", "p", "var", "add", "ANTHROPIC_API_KEY", "--stdin"},
		{"--guest", "secret", "--project", "p", "var", "list"},
	}
	for _, args := range cases {
		code, out, diag := invoke(t, "dummy-sensitive-value", args...)
		if code != 1 || out != "" || strings.Contains(diag, "dummy-sensitive-value") {
			t.Fatalf("unsafe rejection %d %q %q", code, out, diag)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid input used network %d", calls)
	}
}

func TestSecretValueInput(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		bad      bool
	}{{"", "", false}, {"dummy\n", "dummy", false}, {"dummy\r\n", "dummy", false}, {"a\n\n", "", true}, {"a\rb", "", true}, {"a\x00b", "", true}, {strings.Repeat("a", projectsecret.MaxVarBytes+1), "", true}} {
		got, err := readSecretValue(strings.NewReader(tc.in))
		if (err != nil) != tc.bad || !tc.bad && got != tc.want {
			t.Fatalf("value read len=%d err=%v", len(got), err)
		}
	}
}

func secretRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", root)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSecretFileReads(t *testing.T) {
	root := secretRepo(t)
	t.Chdir(root)
	os.Mkdir(filepath.Join(root, "config"), 0700)
	os.WriteFile(filepath.Join(root, "config/key"), []byte("dummy"), 0600)
	files, err := readSecretFiles([]string{"config/key"})
	if err != nil || len(files) != 1 || files[0].Name != "config/key" || string(files[0].Data) != "dummy" {
		t.Fatalf("files %+v %v", files, err)
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "key"), []byte("dummy"), 0600)
	os.Symlink(outside, filepath.Join(root, "linked"))
	os.Symlink("config/key", filepath.Join(root, "link"))
	syscall.Mkfifo(filepath.Join(root, "fifo"), 0600)
	os.WriteFile(filepath.Join(root, "large"), make([]byte, projectsecret.MaxFileBytes+1), 0600)
	for _, name := range []string{"../key", filepath.Join(outside, "key"), "linked/key", "link", "fifo", "config", "large", ".git/config"} {
		if _, err = readSecretFiles([]string{"config/key", name}); err == nil {
			t.Errorf("accepted unsafe file %q", name)
		}
	}
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprint(i)), make([]byte, projectsecret.MaxFileBytes), 0600)
	}
	if files, err := readSecretFiles([]string{"0", "1", "2", "3"}); err != nil || len(files) != 4 {
		t.Fatalf("exact byte boundary rejected: count=%d error=%v", len(files), err)
	}
	if _, err = readSecretFiles([]string{"0", "1", "2", "3", "4"}); err == nil {
		t.Error("accepted total overflow")
	}
}

func TestSecretCLIAuthExpiry(t *testing.T) {
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, "dummy-sensitive-value")
	})
	code, out, diag := invoke(t, "", "secret", "--project", "p", "var", "list")
	if code != 2 || out != "" || strings.Contains(diag, "dummy-sensitive-value") {
		t.Fatalf("expiry %d %q %q", code, out, diag)
	}
}

func TestSecretCLIBatchAndRemove(t *testing.T) {
	root := secretRepo(t)
	t.Chdir(root)
	if err := os.WriteFile("one", []byte("dummy-one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("two", []byte("dummy-two"), 0600); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/projects" {
			fmt.Fprint(w, `[{"id":"p","name":"demo"}]`)
			return
		}
		mutations++
		if r.URL.Path != "/api/projects/p/secrets/files" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Method == "PUT" {
			var body struct {
				Files []projectsecret.File `json:"files"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body.Files) != 2 || body.Files[0].Name != "one" || string(body.Files[1].Data) != "dummy-two" {
				t.Error("incomplete batch")
			}
		} else if r.Method == "DELETE" {
			var body struct {
				Names []string `json:"names"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if strings.Join(body.Names, ",") != "no/source/required" {
				t.Error("logical names not preserved")
			}
		} else {
			t.Error("unexpected method")
		}
		fmt.Fprint(w, `{"revision":1,"files":[],"vars":[]}`)
	})
	code, out, diagnostic := invoke(t, "", "secret", "--project", "p", "file", "add", "one", "missing")
	if code != 1 || out != "" || mutations != 0 || diagnostic == "" {
		t.Fatalf("invalid batch %d %q %q mutations=%d", code, out, diagnostic, mutations)
	}
	code, out, diagnostic = invoke(t, "", "secret", "--project", "p", "file", "add", "one", "two")
	if code != 0 || out != "" || diagnostic != "" || mutations != 1 {
		t.Fatalf("valid batch %d %q %q mutations=%d", code, out, diagnostic, mutations)
	}
	t.Chdir(t.TempDir())
	code, out, diagnostic = invoke(t, "", "secret", "--project", "p", "file", "remove", "no/source/required")
	if code != 0 || out != "" || diagnostic != "" || mutations != 2 {
		t.Fatalf("remove %d %q %q mutations=%d", code, out, diagnostic, mutations)
	}
}

func TestSecretCLIValueFileAndMutationExpiry(t *testing.T) {
	valueFile := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(valueFile, []byte("dummy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/projects" {
			fmt.Fprint(w, `[{"id":"p","name":"demo"}]`)
			return
		}
		var body struct {
			Vars map[string]string `json:"vars"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Vars["TOKEN"] != "dummy" {
			t.Error("value-file content incorrect")
		}
		w.WriteHeader(401)
		fmt.Fprint(w, "dummy-sensitive-value")
	})
	code, out, diagnostic := invoke(t, "ignored", "secret", "--project", "p", "var", "add", "TOKEN", "--value-file", valueFile)
	if code != 2 || out != "" || strings.Contains(diagnostic, "dummy-sensitive-value") {
		t.Fatalf("mutation expiry %d %q %q", code, out, diagnostic)
	}
}

func TestSecretProjectResolution(t *testing.T) {
	projects := []clientapi.Project{{ID: "p1", Name: "demo"}, {ID: "p2", Name: "demo"}, {ID: "p3", Name: "p1"}}
	if id, err := resolveSecretProject(projects, "p1"); err != nil || id != "p1" {
		t.Fatalf("ID precedence %q %v", id, err)
	}
	for _, name := range []string{"", "demo", "missing"} {
		if _, err := resolveSecretProject(projects, name); err == nil {
			t.Error("ambiguous/missing project accepted")
		}
	}
}
