package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

const cliGitToken = "synthetic-git-token-sentinel"

// FEATURE: bro secret git manages server-only project Git credentials (#173).
// SPEC: The token is read only from stdin or a private regular file (no
// group or other permissions, not a symlink or FIFO), never argv; the repository is canonicalized and bound; list prints names,
// repositories and the active state; mutations print nothing; refusals never
// echo the token and invalid input never reaches the server.
// IMPLEMENTATION: Explicit parsing, shared validators, normal session client.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestSecretGitCLI(t *testing.T) {
	var puts []map[string]string
	var deletes [][]string
	secretServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/projects":
			fmt.Fprint(w, `[{"id":"p1","name":"demo"}]`)
			return
		case r.URL.Path != "/api/projects/p1/secrets/git":
			t.Errorf("unexpected path %s", r.URL.Path)
		case r.Method == "PUT":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			puts = append(puts, body)
		case r.Method == "DELETE":
			var body struct {
				Names []string `json:"names"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			deletes = append(deletes, body.Names)
		}
		fmt.Fprint(w, `{"items":[{"name":"gitlab","repository":"https://git.example.com/group/repo.git","active":true},{"name":"spare","repository":"https://git.example.com/group/other.git","active":false}]}`)
	})
	code, out, diag := invoke(t, cliGitToken+"\n", "secret", "--project", "demo", "git", "add", "gitlab", "--repository", "git@Git.Example.com:group/repo.git", "--username", "brood-bot", "--stdin")
	if code != 0 || out != "" || diag != "" {
		t.Fatalf("add from stdin: %d %q %q", code, out, diag)
	}
	valueFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(valueFile, []byte(cliGitToken+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, diag = invoke(t, "ignored", "secret", "--project", "p1", "git", "add", "spare", "--username", "brood-bot", "--value-file", valueFile, "--repository", "https://git.example.com/group/other.git")
	if code != 0 || out != "" || diag != "" {
		t.Fatalf("add from file: %d %q %q", code, out, diag)
	}
	want := []map[string]string{
		{"name": "gitlab", "repository": "https://git.example.com/group/repo.git", "username": "brood-bot", "token": cliGitToken},
		{"name": "spare", "repository": "https://git.example.com/group/other.git", "username": "brood-bot", "token": cliGitToken},
	}
	if !reflect.DeepEqual(puts, want) {
		t.Fatalf("PUT bodies = %v", puts)
	}
	code, out, diag = invoke(t, "", "secret", "--project", "demo", "git", "list")
	if code != 0 || out != "gitlab\thttps://git.example.com/group/repo.git\tactive\nspare\thttps://git.example.com/group/other.git\tinactive\n" || diag != "" {
		t.Fatalf("list: %d %q %q", code, out, diag)
	}
	code, out, diag = invoke(t, "", "secret", "--project", "demo", "git", "remove", "gitlab", "spare")
	if code != 0 || out != "" || diag != "" || !reflect.DeepEqual(deletes, [][]string{{"gitlab", "spare"}}) {
		t.Fatalf("remove: %d %q %q %v", code, out, diag, deletes)
	}
}

func TestSecretGitCLIRejectsUnsafeInput(t *testing.T) {
	calls := 0
	secretServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, `[]`) })
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	fifo := filepath.Join(dir, "fifo")
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(cliGitToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "token"), link); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(dir, "shared")
	if err := os.WriteFile(shared, []byte(cliGitToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o640); err != nil {
		t.Fatal(err)
	}
	add := []string{"secret", "--project", "p", "git", "add", "gitlab", "--repository", "https://git.example.com/group/repo.git", "--username", "brood-bot"}
	for _, tc := range []struct {
		stdin string
		args  []string
	}{
		{cliGitToken, append(append([]string{}, add...), cliGitToken)},
		{cliGitToken, append(append([]string{}, add...), "--token", cliGitToken)},
		{cliGitToken, append(append([]string{}, add...), "--stdin", "--value-file", filepath.Join(dir, "token"))},
		{cliGitToken, add},
		{cliGitToken, []string{"secret", "--project", "p", "git", "add", "gitlab", "--username", "brood-bot", "--stdin"}},
		{cliGitToken, []string{"secret", "--project", "p", "git", "add", "gitlab", "--repository", "https://git.example.com/group/repo.git", "--stdin"}},
		{cliGitToken, []string{"secret", "--project", "p", "git", "add", "gitlab", "--repository", "https://brood-bot:" + cliGitToken + "@git.example.com/group/repo.git", "--username", "brood-bot", "--stdin"}},
		{cliGitToken, []string{"secret", "--project", "p", "git", "add", "gitlab", "--repository", "https://git.example.com/group/repo.git", "--username", "brood:" + cliGitToken, "--stdin"}},
		{cliGitToken, []string{"secret", "--project", "p", "git", "add", "Bad", "--repository", "https://git.example.com/group/repo.git", "--username", "brood-bot", "--stdin"}},
		{cliGitToken, []string{"secret", "--project", "p", "git", "add", "one", "two", "--repository", "https://git.example.com/group/repo.git", "--username", "brood-bot", "--stdin"}},
		{"", append(append([]string{}, add...), "--stdin")},
		{cliGitToken + "\n\n", append(append([]string{}, add...), "--stdin")},
		{cliGitToken + " x", append(append([]string{}, add...), "--stdin")},
		{strings.Repeat("t", 4097), append(append([]string{}, add...), "--stdin")},
		{"", append(append([]string{}, add...), "--value-file", link)},
		{"", append(append([]string{}, add...), "--value-file", fifo)},
		{"", append(append([]string{}, add...), "--value-file", shared)},
		{"", []string{"secret", "--project", "p", "git", "list", "--repository", "https://git.example.com/group/repo.git"}},
		{"", []string{"secret", "--project", "p", "git", "list", "gitlab"}},
		{"", []string{"secret", "--project", "p", "git", "remove"}},
		{"", []string{"secret", "--project", "p", "git", "remove", "gitlab", "--username", "brood-bot"}},
		{"", []string{"secret", "--project", "p", "var", "add", "TOKEN", "--stdin", "--repository", "https://git.example.com/group/repo.git"}},
		{"", []string{"secret", "git", "list"}},
		{"", []string{"--guest", "secret", "--project", "p", "git", "list"}},
	} {
		code, out, diag := invoke(t, tc.stdin, tc.args...)
		if code != 1 || out != "" || strings.Contains(diag, cliGitToken) {
			t.Fatalf("%v: %d %q %q", tc.args, code, out, diag)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid input used the network %d times", calls)
	}
}
