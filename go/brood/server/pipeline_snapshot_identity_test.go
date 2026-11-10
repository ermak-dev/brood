package server

// FEATURE: Личность снимка на проводе роя (#218).
// SPEC: GET /api/pipeline-runs/{id} и список /api/swarms/{id}/pipeline-runs
// несут definition {name, version, revision} — личность снимка, который
// записал первый запуск, а не текущего определения каталога. Новая
// публикация определения меняет каталог и не меняет отображение уже
// запущенного роя: расхождение снимка с каталогом видно, а не угадывается.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPipelineRunSnapshotIdentityWire(t *testing.T) {
	f := startAPISetup(t)
	ctx := context.Background()
	r, b := f.start(f.alice, map[string]any{"definition": "release", "commit": startAPICommit})
	expectStatus(t, "start", r, b, http.StatusOK)
	pinned, err := f.ts.srv.store.GetWorkflowDefinition(ctx, f.project, "release")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := map[string]any{"name": "release", "version": float64(pinned.Version), "revision": pinned.Revision}
	identity := func(what string, body map[string]any) {
		t.Helper()
		got, _ := body["definition"].(map[string]any)
		if len(got) != 3 || got["name"] != snapshot["name"] || got["version"] != snapshot["version"] || got["revision"] != snapshot["revision"] {
			t.Fatalf("%s: definition = %v, want snapshot %v", what, body["definition"], snapshot)
		}
	}
	r, b, _ = f.ts.do(f.alice, "GET", "/api/pipeline-runs/"+f.swarm, "", nil)
	expectStatus(t, "detail", r, b, http.StatusOK)
	identity("detail after start", b)

	// Новая версия того же определения: каталог уходит вперёд, рой остаётся
	// на своём снимке.
	root, err := f.ts.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	next := strings.Replace(startAPIRelease, "question: Ship?", "question: Ship now?", 1)
	if _, err := f.ts.srv.store.SyncWorkflowDefinitions(ctx, f.project, root.ID, map[string]string{"release": next, "approve": startAPIApprove}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	published, err := f.ts.srv.store.GetWorkflowDefinition(ctx, f.project, "release")
	if err != nil {
		t.Fatal(err)
	}
	if published.Version == pinned.Version || published.Revision == pinned.Revision {
		t.Fatalf("publication did not move the catalog: %+v after %+v", published, pinned)
	}
	r, b, _ = f.ts.do(f.alice, "GET", "/api/pipeline-runs/"+f.swarm, "", nil)
	expectStatus(t, "detail after publication", r, b, http.StatusOK)
	identity("detail after publication", b)

	r, _, raw := f.ts.do(f.alice, "GET", "/api/swarms/"+f.swarm+"/pipeline-runs", "", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, body %s", r.StatusCode, raw)
	}
	var runs []map[string]any
	if err := json.Unmarshal([]byte(raw), &runs); err != nil || len(runs) != 1 {
		t.Fatalf("list: %s %v", raw, err)
	}
	identity("list after publication", runs[0])
}
