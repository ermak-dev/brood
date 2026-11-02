package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/clientapi"
	catalog "brood.dev/go/brood/command"
)

// FEATURE: bro CLI and MCP share catalog parsing and explicit execution scope.
// SPEC: Real stdio discovery needs no broker; operator calls use scoped web API.
// IMPLEMENTATION: Catalog paths and generated help share explicit adapter selection.
// RESULT: IMPLEMENTED — isolated CLI streams and real loopback HTTP pass.
// STATUS: DONE
func TestCLIHelpUsesCatalog(t *testing.T) {
	code, out, diagnostic := invoke(t, "", "help")
	if code != 0 || diagnostic != "" {
		t.Fatalf("help: %d %s", code, diagnostic)
	}
	for _, spec := range catalog.Specs() {
		if !strings.Contains(out, "  "+strings.Join(spec.Path, " ")+" ") {
			t.Errorf("help missing %v", spec.Path)
		}
	}
}

func TestCLIGuestMCPDiscovery(t *testing.T) {
	isolateState(t)
	t.Setenv("BROOD_EXECUTION_MODE", "")
	t.Setenv("BROOD_SWARM", "")
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n"
	code, out, diagnostic := invoke(t, input, "--guest", "mcp")
	if code != 0 || diagnostic != "" || !strings.Contains(out, `"name":"agents"`) || !strings.Contains(out, "mcp__brood__send") {
		t.Fatalf("code=%d out=%s err=%s", code, out, diagnostic)
	}
}

func TestCLICatalogOperatorCommand(t *testing.T) {
	isolateState(t)
	t.Setenv("BROOD_EXECUTION_MODE", "")
	t.Setenv("BROOD_SWARM", "")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/commands/send" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Swarm     string          `json:"swarm_id"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		want, err := catalog.ParseCLI([]string{"send", "peer", "hello", "--wait-seconds", "2"})
		if err != nil {
			t.Fatal(err)
		}
		if body.Swarm != "s1" || string(body.Arguments) != string(want.Arguments) {
			t.Errorf("body: %+v", body)
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"text":"queued","outcome":"accepted"}`)
	}))
	defer server.Close()
	store, err := clientapi.NewSessionStore(filepath.Join(os.Getenv("XDG_STATE_HOME"), "brood", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetCurrent(server.URL); err != nil {
		t.Fatal(err)
	}
	if err = store.Save(server.URL, clientapi.StoredSession{Session: "session", CSRF: "csrf"}); err != nil {
		t.Fatal(err)
	}
	code, out, diagnostic := invoke(t, "", "send", "peer", "hello", "--wait-seconds", "2", "--swarm", "s1")
	if code != 0 || calls != 1 || !strings.Contains(out, "accepted") || strings.Contains(out, "completed") || diagnostic != "" {
		t.Fatalf("code=%d calls=%d out=%q err=%q", code, calls, out, diagnostic)
	}
}
