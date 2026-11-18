package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// TestUI305HistoryBrowser exposes real request history and the normal embedded
// panel with synthetic providers and a temporary database, only when opted in.
func TestUI305HistoryBrowser(t *testing.T) {
	descriptor := os.Getenv("BROOD_UI305_DESCRIPTOR")
	if descriptor == "" {
		t.Skip("explicit isolated browser acceptance only")
	}
	rs := newRequestServer(t)
	ctx := context.Background()
	alice, err := rs.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, question := range []string{"Cancelled history first", "Cancelled history second"} {
		id := rs.mustAsk(t, map[string]any{"question": question})
		if err := rs.srv.store.CancelSwarmRequest(ctx, rs.master.SwarmID, id, alice.ID, func(context.Context) bool { return true }); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	var once sync.Once
	var mixed sync.Once
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/mixed":
			mixed.Do(func() {
				id := rs.mustAsk(t, map[string]any{"question": "Answered history evidence"})
				response, body, _ := rs.do(rs.alice, "POST", rs.answerPath(rs.master.SwarmID, id), rs.csrf(rs.alice), map[string]any{"response": "Synthetic answer evidence"})
				expectStatus(t, "answer fixture request", response, body, http.StatusOK)
			})
		case "/done":
			once.Do(func() { close(done) })
		default:
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(control.Close)
	data, err := json.Marshal(map[string]string{"origin": rs.http.URL, "control": control.URL, "swarm": rs.master.SwarmID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(descriptor, data, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Minute):
		t.Fatal("history browser acceptance timed out")
	}
}
