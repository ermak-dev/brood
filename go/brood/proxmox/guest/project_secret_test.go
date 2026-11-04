//go:build linux

package guest

// FEATURE: Managed project secrets reach only the final unprivileged app.
// SPEC: Bounded authenticated HTTPS fetch, root projection, safe atomic file
// delivery, unchanged-only removal, fixed app entry and pre-stop refresh.
// IMPLEMENTATION: Root HTTPS staging and descriptor-based app delivery; fixed
// launch entry and connection-owned refresh gate, with a hash-only manifest.
// RESULT: IMPLEMENTED — fetch, removal, conflicts, FIFO/symlink rejection and
// launch boundaries verified locally without root services or real secrets.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"brood.dev/go/brood/projectsecret"
	"brood.dev/go/brood/proxmox/relay"
	"golang.org/x/sys/unix"
)

func TestProjectSecretFetch(t *testing.T) {
	want := projectsecret.Bundle{Revision: 2, Files: []projectsecret.File{{Name: "config/key", Data: []byte("opaque")}}, Vars: map[string]string{"SERVICE_KEY": "one=two"}}
	mode := "ok"
	redirected := false
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer sink.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/proxmox/secrets/actor-1/3" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 43) {
			t.Error("incorrect bootstrap identity")
		}
		switch mode {
		case "redirect":
			http.Redirect(w, r, sink.URL, http.StatusFound)
		case "large":
			_, _ = w.Write([]byte(strings.Repeat(" ", (4<<20)+1)))
		case "unknown":
			_, _ = w.Write([]byte(`{"revision":1,"token":"opaque"}`))
		case "invalid":
			_, _ = w.Write([]byte(`{"revision":1,"vars":{"LD_PRELOAD":"opaque"}}`))
		case "error":
			http.Error(w, "opaque", 403)
		default:
			_ = json.NewEncoder(w).Encode(want)
		}
	}))
	defer server.Close()
	cfg := relay.ClientConfig{URL: strings.Replace(server.URL, "https:", "wss:", 1) + "/api/proxmox/guest/actor-1/3", ActorUID: "actor-1", Generation: 3, Token: strings.Repeat("a", 43), CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	got, err := FetchProjectSecrets(context.Background(), cfg)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("fetch failed: %v", err)
	}
	for _, m := range []string{"redirect", "large", "unknown", "invalid", "error"} {
		mode = m
		if _, err := FetchProjectSecrets(context.Background(), cfg); err == nil || strings.Contains(err.Error(), "opaque") || strings.Contains(err.Error(), cfg.Token) {
			t.Fatalf("unsafe fetch failure for %s", m)
		}
	}
	if redirected {
		t.Fatal("followed redirect")
	}
	cfg.CAPEM = nil
	if _, err := FetchProjectSecrets(context.Background(), cfg); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
}

func TestProjectSecretApplyAndRemoval(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	secretTestGit(t, root, "init", "-q")
	bundle := projectsecret.Bundle{Revision: 1, Files: []projectsecret.File{{Name: "nested/key", Data: []byte("first")}}, Vars: map[string]string{"SERVICE_KEY": "literal=$(false)"}}
	env, err := applyProjectSecrets(root, home, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(env, "\n"), "SERVICE_KEY=literal=$(false)") {
		t.Fatal("approved value absent")
	}
	for _, p := range []string{filepath.Join(root, "nested/key"), filepath.Join(home, ".config/brood/project-secrets.json")} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatal("insecure delivery", err)
		}
	}
	manifest, _ := os.ReadFile(filepath.Join(home, ".config/brood/project-secrets.json"))
	if strings.Contains(string(manifest), "first") || strings.Contains(string(manifest), "SERVICE_KEY") {
		t.Fatal("manifest contains values")
	}
	bundle.Files[0].Data = []byte("second")
	if _, err = applyProjectSecrets(root, home, bundle); err != nil {
		t.Fatal(err)
	}
	env, err = applyProjectSecrets(root, home, projectsecret.Bundle{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(root, "nested/key")); !os.IsNotExist(err) {
		t.Fatal("removed file remains")
	}
	if strings.Contains(strings.Join(env, "\n"), "SERVICE_KEY=") {
		t.Fatal("old environment remains")
	}
	if _, err = applyProjectSecrets(root, home, bundle); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "nested/key"), []byte("user edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = applyProjectSecrets(root, home, projectsecret.Bundle{}); err == nil {
		t.Fatal("changed owned file silently removed")
	}
	data, _ := os.ReadFile(filepath.Join(root, "nested/key"))
	if string(data) != "user edit" {
		t.Fatal("user changes lost")
	}
}

func TestProjectSecretManifestBoundBeforeFiles(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	parent := strings.Repeat("d", 240) + "/" + strings.Repeat("e", 240) + "/" + strings.Repeat("f", 240)
	bundle := projectsecret.Bundle{}
	for i := 0; i < 80; i++ {
		bundle.Files = append(bundle.Files, projectsecret.File{Name: parent + "/" + strings.Repeat("k", 237) + strconv.Itoa(i), Data: []byte("opaque")})
	}
	if _, err := applyProjectSecrets(root, home, bundle); err == nil {
		t.Fatal("unreadable oversized manifest published")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("oversized manifest rejected only after file effects")
	}
}

func TestProjectSecretRefusesUnsafePaths(t *testing.T) {
	for _, kind := range []string{"symlink-parent", "symlink-file", "user-file", "hardlink", "fifo", "control", "manifest-symlink", "home-symlink", "git-symlink", "git-info-symlink", "git-exclude-symlink"} {
		t.Run(kind, func(t *testing.T) {
			root, home, out := t.TempDir(), t.TempDir(), t.TempDir()
			if !strings.HasPrefix(kind, "git-") {
				secretTestGit(t, root, "init", "-q")
			}
			name := "key"
			switch kind {
			case "symlink-parent":
				_ = os.Symlink(out, filepath.Join(root, "dir"))
				name = "dir/key"
			case "symlink-file":
				_ = os.Symlink(filepath.Join(out, "key"), filepath.Join(root, "key"))
			case "user-file":
				_ = os.WriteFile(filepath.Join(root, "key"), []byte("user"), 0600)
			case "hardlink":
				_ = os.WriteFile(filepath.Join(out, "key"), []byte("user"), 0600)
				_ = os.Link(filepath.Join(out, "key"), filepath.Join(root, "key"))
			case "fifo":
				if err := unix.Mkfifo(filepath.Join(root, "key"), 0600); err != nil {
					t.Fatal(err)
				}
			case "git-symlink":
				if err := os.Symlink(out, filepath.Join(root, ".git")); err != nil {
					t.Fatal(err)
				}
			case "git-info-symlink":
				if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(out, filepath.Join(root, ".git/info")); err != nil {
					t.Fatal(err)
				}
			case "git-exclude-symlink":
				if err := os.MkdirAll(filepath.Join(root, ".git/info"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(out, "key"), filepath.Join(root, ".git/info/exclude")); err != nil {
					t.Fatal(err)
				}
			case "control":
				name = ".git/config"
			case "manifest-symlink":
				_ = os.MkdirAll(filepath.Join(home, ".config/brood"), 0700)
				_ = os.Symlink(filepath.Join(out, "key"), filepath.Join(home, ".config/brood/project-secrets.json"))
			case "home-symlink":
				_ = os.Symlink(out, filepath.Join(home, ".config"))
			}
			_, err := applyProjectSecrets(root, home, projectsecret.Bundle{Files: []projectsecret.File{{Name: name, Data: []byte("opaque")}}})
			if err == nil || strings.Contains(err.Error(), "opaque") {
				t.Fatal("unsafe delivery accepted")
			}
			if data, _ := os.ReadFile(filepath.Join(out, "key")); string(data) == "opaque" {
				t.Fatal("escaped workspace")
			}
		})
	}
}

func TestProjectSecretGitExcludesAndCleanReplay(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	secretTestGit(t, root, "init", "-q")
	for name, text := range map[string]string{".gitignore": "user-rules\n", ".git/info/exclude": "existing-pattern\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	secretTestGit(t, root, "add", "--", ".gitignore")
	bundle := projectsecret.Bundle{Files: []projectsecret.File{{Name: "key[1]", Data: []byte("opaque")}}}
	for i := 0; i < 2; i++ {
		if _, err := applyProjectSecrets(root, home, bundle); err != nil {
			t.Fatal(err)
		}
		// git clean -ffdx удаляет и ignored файлы перед новым app entry.
		secretTestGit(t, root, "clean", "-ffdx")
		if _, err := os.Stat(filepath.Join(root, "key[1]")); !os.IsNotExist(err) {
			t.Fatal("real git clean did not remove ignored secret")
		}
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil || string(data) != "user-rules\n" {
		t.Fatal("user ignore changed")
	}
	data, err = os.ReadFile(filepath.Join(root, ".git/info/exclude"))
	if err != nil || !strings.HasPrefix(string(data), "existing-pattern\n") || strings.Count(string(data), "/key\\[1\\]\n") != 1 {
		t.Fatal("exclude not preserved and deduplicated")
	}
	if _, err = applyProjectSecrets(root, home, projectsecret.Bundle{}); err != nil {
		t.Fatal("removed file after clean rejected", err)
	}
}

func TestProjectSecretProjectionSafety(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	bundle := projectsecret.Bundle{Revision: 1, Vars: map[string]string{"SERVICE_KEY": "opaque"}}
	// Тестирует тот же descriptor publisher без требования root в тестовом процессе.
	if err := publishProjectSecrets(dir, bundle, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "project-secrets.json")
	st, err := os.Stat(file)
	if err != nil || st.Mode().Perm() != 0440 {
		t.Fatal("projection permissions", err)
	}
	got, err := readProjectSecrets(dir, os.Geteuid(), os.Getegid())
	if err != nil || !reflect.DeepEqual(got, bundle) {
		t.Fatal("projection roundtrip", err)
	}
	if err = publishProjectSecrets(dir, projectsecret.Bundle{}, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	got, err = readProjectSecrets(dir, os.Geteuid(), os.Getegid())
	if err != nil || len(got.Vars) != 0 {
		t.Fatal("empty stage did not clear vars")
	}
	_ = os.Remove(file)
	_ = os.Symlink(filepath.Join(t.TempDir(), "target"), file)
	if err = publishProjectSecrets(dir, bundle, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("projection symlink accepted")
	}
	if _, err = readProjectSecrets(dir, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("projection symlink read")
	}
	_ = os.Remove(file)
	_ = os.Chmod(dir, 0777)
	if err = publishProjectSecrets(dir, bundle, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("writable projection root accepted")
	}
}

func TestProjectSecretAppLaunchContract(t *testing.T) {
	if !strings.Contains(strings.Join(actorPTYConfig("/opt/brood/bin/brood-guest").Command, " "), "/opt/brood/bin/brood-guest --brood-guest-app") {
		t.Fatal("cold start bypasses app secret entry")
	}
	env, err := projectAppEnvironment(projectsecret.Bundle{Vars: map[string]string{"HOME": "/evil"}})
	if err == nil || env != nil {
		t.Fatal("reserved variable accepted")
	}
}

func TestProjectSecretRefreshBeforeSessionHelper(t *testing.T) {
	for _, op := range []string{"restart", "recycle"} {
		var events []string
		connection, cancel := context.WithCancel(context.Background())
		refresh := func(context.Context) error { events = append(events, "refresh"); return errors.New("opaque") }
		run := func(context.Context, relay.SessionRequest) relay.SessionResponse {
			events = append(events, "helper")
			return relay.SessionResponse{Outcome: "completed"}
		}
		h := newSessionHandlerWithSecrets(connection, refresh, run)
		response := h(context.Background(), relay.SessionRequest{Operation: op, Arguments: json.RawMessage(`{"confirm":true}`)})
		if response.Outcome != "failed" || strings.Contains(response.Text, "opaque") || !reflect.DeepEqual(events, []string{"refresh"}) {
			t.Fatal("old session touched after refresh failure", events, response)
		}
		cancel()
		events = nil
		h(context.Background(), relay.SessionRequest{Operation: op})
		if len(events) != 0 {
			t.Fatal("closed connection refreshed")
		}
	}
}
