package server

import (
	"context"
	"encoding/json"
	"testing"
)

// BUG: Ошибочный рой не сообщает панели, какую операцию можно повторить (#49).
// HYPOTHESIS: HTTP DTO теряет сохранённый retry intent при проекции роя.
func TestSwarmHTTPPreservesRetryIntent(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, _, swarm := actorsSetup(t, ts.testServer)
	row, err := ts.srv.store.GetSwarm(context.Background(), swarm)
	if err != nil {
		t.Fatal(err)
	}
	for _, retry := range []string{"create", "delete", ""} {
		state := "error"
		if retry == "" {
			state = "active"
		}
		if err := ts.srv.store.SetSwarmProgress(context.Background(), swarm, state, retry); err != nil {
			t.Fatal(err)
		}
		r, body, _ := ts.do(alice, "GET", "/api/swarms/"+swarm, "", nil)
		expectStatus(t, "get retry intent", r, body, 200)
		if body["retry"] != retry {
			t.Fatalf("detail retry = %v, want %q", body["retry"], retry)
		}
		r, body, raw := ts.do(alice, "GET", "/api/projects/"+row.ProjectID+"/swarms", "", nil)
		expectStatus(t, "list retry intent", r, body, 200)
		var items []map[string]any
		if err := json.Unmarshal([]byte(raw), &items); err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0]["retry"] != retry {
			t.Fatalf("list retry intent = %s, want %q", raw, retry)
		}
	}
}
