package clientapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"brood.dev/go/brood/command"
)

// FEATURE: Operator commands reuse the authenticated web session.
// SPEC: Scoped POST with CSRF, raw arguments, and distinct accepted outcome.
// IMPLEMENTATION: CommandExecutor reuses Client with HTTP 202 result support.
// RESULT: IMPLEMENTED — loopback requests verify session, CSRF, scope and outcomes.
// STATUS: DONE
func TestCommandExecutorScopedPost(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusAccepted} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/api/commands/agents" || r.Header.Get(headerCSRF) != "csrf-fixture" {
					t.Errorf("request %s %s", r.Method, r.URL.Path)
				}
				ck, err := r.Cookie(cookieSession)
				if err != nil || ck.Value != "session-fixture" {
					t.Error("missing session")
				}
				var body struct {
					Swarm     string          `json:"swarm_id"`
					Arguments json.RawMessage `json:"arguments"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Swarm != "swarm-1" || string(body.Arguments) != "{}" {
					t.Errorf("body: %+v", body)
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"text":"queued","outcome":"accepted"}`))
			}))
			defer server.Close()
			c, err := New(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			c.Restore(StoredSession{Session: "session-fixture", CSRF: "csrf-fixture"})
			result, err := (CommandExecutor{Client: c, SwarmID: "swarm-1"}).Execute(context.Background(), command.Invocation{Name: "agents", Arguments: json.RawMessage(`{}`)})
			if err != nil || result.Outcome != "accepted" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}
