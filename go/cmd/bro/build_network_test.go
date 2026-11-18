package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/egress"
)

func TestBuildPublishesExplicitManagedOfflinePolicyBeforeDocker(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	addBuildOrigin(t, root)
	t.Chdir(root)
	file := filepath.Join(root, "Broodfile")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(data, []byte("project:\n  managed_network: offline\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/api/projects/sync" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != "fixture-session" || r.Header.Get("X-CSRFToken") != "fixture-csrf" {
			t.Error("selected policy sync lost authenticated transport")
		}
		var declaration map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&declaration); err != nil {
			t.Error(err)
		}
		policy, err := egress.Decode(declaration["managed_network"])
		if err != nil || policy.AllowsDNS("example.com") {
			t.Errorf("explicit offline declaration lost: %v", err)
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"project-id","name":"example","repository":"https://git.example.com/team/current.git","created":false,"changed":true,"origin_mode":"gateway"}`))
	}))
	defer server.Close()
	saveBuildSession(t, server.URL)
	log := fakeDocker(t, "printf 'network fixture build failed\\n' >&2\nexit 42\n")
	code, _, diagnostic := invoke(t, "", "build")
	if code == 0 || !strings.Contains(diagnostic, "network fixture build failed") || calls != 1 {
		t.Fatalf("policy did not reach sync before Docker: code=%d calls=%d diagnostic=%q", code, calls, diagnostic)
	}
	if _, err := os.Stat(log); err != nil {
		t.Fatal("build did not reach the isolated Docker fixture")
	}
}
