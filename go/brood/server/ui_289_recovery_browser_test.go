package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// TestUI289RecoveryBrowser serves the normal embedded panel and real API over a
// temporary SQLite database. Existing test helpers isolate all providers/relays.
func TestUI289RecoveryBrowser(t *testing.T) {
	descriptor := os.Getenv("BROOD_UI289_DESCRIPTOR")
	if descriptor == "" {
		t.Skip("explicit isolated browser acceptance only")
	}
	d := newChildEngineServer(t)
	ctx := context.Background()
	bob, err := d.srv.store.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.srv.store.SetSwarmMember(ctx, d.swarm, bob.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var once sync.Once
	var childID string
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/done":
			once.Do(func() { close(done) })
			w.WriteHeader(204)
			return
		case "/create":
			args := childArgs(d, "ui289-browser")
			args.Task = "<script>window.childPayloadExecuted=true</script> Preserved task evidence."
			child, err := d.srv.managed.createChild(ctx, d.master, args)
			if err != nil {
				t.Error(err)
				http.Error(w, err.Error(), 500)
				return
			}
			childID = child.ID
		case "/owner-only":
			alice, err := d.srv.store.GetUserByUsername(ctx, "alice")
			if err != nil {
				t.Error(err)
				http.Error(w, err.Error(), 500)
				return
			}
			if err := d.srv.store.SetUserAdmin(ctx, alice.ID, false); err != nil {
				t.Error(err)
				http.Error(w, err.Error(), 500)
				return
			}
		case "/dispatch":
			d.srv.children.dispatch(ctx)
		case "/complete":
			if err := d.srv.managed.completeChild(ctx, d.worker, childID, "ui289-browser", "fail", "Independent failure report."); err != nil {
				t.Error(err)
				http.Error(w, err.Error(), 500)
				return
			}
		case "/state":
		default:
			http.NotFound(w, r)
			return
		}
		db, err := sql.Open("sqlite", d.dbPath)
		if err != nil {
			t.Error(err)
			return
		}
		defer db.Close()
		var schema int
		if err := db.QueryRow("PRAGMA user_version").Scan(&schema); err != nil {
			t.Error(err)
		}
		sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
		if err != nil {
			t.Error(err)
			return
		}
		group, groupErr := d.srv.store.GetSwarmChildHistory(ctx, d.swarm, 1)
		json.NewEncoder(w).Encode(map[string]any{"schema": schema, "step": d.committed().State, "execution": sw.Execution, "group": group, "groupMissing": groupErr != nil, "workerUid": d.worker.UID, "workerIncarnation": d.worker.Incarnation})
	}))
	t.Cleanup(control.Close)
	data, _ := json.Marshal(map[string]string{"origin": d.http.URL, "control": control.URL, "swarm": d.swarm})
	if err := os.WriteFile(descriptor, data, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Minute):
		t.Fatal("browser acceptance timed out")
	}
}
