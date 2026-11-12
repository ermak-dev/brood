package main

// BUG: bro --swarm S update worker2 printed only "server error (status 409):
// Conflict" while the server named why it refused (#276).
// SPEC: bro prints the server's public refusal reason after the status, on
// the CLI and, as the text of a failed tool result, in operator MCP mode; the
// untrusted detail stays hidden.
// IMPLEMENTATION: operatorExecutor over clientapi.CommandExecutor.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/clientapi"
)

const worker2Refusal = `update refused: actor "worker2" did not report its current guest release (its guest runtime predates guest_release), and update does not restart a container whose release it cannot check; a guest runtime older than #133 cannot report it: a one-time stop and start installs one that can, but only in a container created from a template with the bootstrap of #184; a container from an earlier template must not be stopped or restarted after a deploy that changed the guest artifacts (docs/PROXMOX.md)`

// refusingServer отвечает на update отказом сервера #276 и запоминает сеанс.
func refusingServer(t *testing.T) {
	t.Helper()
	isolateState(t)
	t.Setenv("BROOD_EXECUTION_MODE", "")
	t.Setenv("BROOD_SWARM", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/commands/update" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		data, _ := json.Marshal(map[string]string{"detail": "conflict: " + worker2Refusal + " synthetic-detail", "reason": worker2Refusal})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write(data)
	}))
	t.Cleanup(server.Close)
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
}

func TestCLIShowsCommandRefusal(t *testing.T) {
	refusingServer(t)
	code, out, diagnostic := invoke(t, "", "--swarm", "s1", "update", "worker2")
	want := "bro update: POST /api/commands/update: server error (status 409): Conflict: " + worker2Refusal + "\n"
	if code != 1 || out != "" || diagnostic != want {
		t.Fatalf("code=%d out=%q\ndiagnostic %q\nwant       %q", code, out, diagnostic, want)
	}
}

func TestOperatorMCPShowsCommandRefusal(t *testing.T) {
	refusingServer(t)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"update","arguments":{"name":"worker2"}}}`,
	}, "\n") + "\n"
	code, out, diagnostic := invoke(t, input, "--swarm", "s1", "mcp")
	if code != 0 || diagnostic != "" {
		t.Fatalf("code=%d diagnostic=%q", code, diagnostic)
	}
	decoder := json.NewDecoder(strings.NewReader(out))
	var call struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
	}
	for call.ID != 2 {
		if err := decoder.Decode(&call); err == io.EOF {
			t.Fatalf("no tools/call response: %s", out)
		} else if err != nil {
			t.Fatal(err)
		}
	}
	want := "POST /api/commands/update: server error (status 409): Conflict: " + worker2Refusal
	if !call.Result.IsError || len(call.Result.Content) != 1 || call.Result.Content[0].Text != want {
		t.Fatalf("tool result %+v\nwant error text %q", call.Result, want)
	}
}
