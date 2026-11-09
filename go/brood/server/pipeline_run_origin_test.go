package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: The pipeline wire names the graph's swarm, project, owner and
// stage actors (#191, #206).
// SPEC: List, detail and answer responses carry swarmId, swarmName, projectId,
// projectName and the owner username of the run's own swarm — the swarm is
// the run. A visit carries actorName from the native reservation or the
// managed actor of the same swarm, and omits it otherwise. Callers who
// cannot read the swarm still get the same 404 as for a missing run.
// IMPLEMENTATION: Render the swarm row; actor names come from a
// swarm-scoped step read.
// RESULT: IMPLEMENTED — the panel can link a run to its swarm, project and actors.
// STATUS: DONE
func TestPipelineRunOriginWire(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	admin, alice, bob, swarm := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	sw, err := ts.srv.store.GetSwarm(ctx, swarm)
	if err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "other"})
	expectStatus(t, "other project", r, b, http.StatusCreated)
	other, _ := b["id"].(string)
	r, b, _ = ts.do(admin, "POST", "/api/projects/"+other+"/members", ts.csrf(admin), map[string]string{"username": "bob"})
	expectStatus(t, "other member", r, b, http.StatusNoContent)
	r, b, _ = ts.do(bob, "POST", "/api/projects/"+other+"/swarms", ts.csrf(bob), map[string]string{"name": "beta"})
	expectStatus(t, "beta swarm", r, b, http.StatusCreated)
	beta, _ := b["id"].(string)
	if err := insertSwarmGraph(ctx, ts.testServer, swarm, []byte(pipelineAPIGraph), "waiting"); err != nil {
		t.Fatal(err)
	}
	if err := insertSwarmGraph(ctx, ts.testServer, beta, []byte(pipelineAPIGraph), "waiting"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{swarm, beta} {
		if _, err := ts.srv.store.AppendPipelineVisit(ctx, id, id, store.PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
			t.Fatal(err)
		}
	}
	alpha := map[string]any{"id": swarm, "swarmId": swarm, "swarmName": "alpha", "projectId": sw.ProjectID, "projectName": "proj", "owner": "alice"}
	betaOrigin := map[string]any{"id": beta, "swarmId": beta, "swarmName": "beta", "projectId": other, "projectName": "other", "owner": "bob"}
	origin := func(name string, got, want map[string]any) {
		t.Helper()
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s: %s=%v, want %v (%v)", name, k, got[k], v, got)
			}
		}
	}
	list := func(client *http.Client, id string) []map[string]any {
		t.Helper()
		r, b, raw := ts.do(client, "GET", "/api/swarms/"+id+"/pipeline-runs", "", nil)
		expectStatus(t, "list", r, b, http.StatusOK)
		var runs []map[string]any
		if err := json.Unmarshal([]byte(raw), &runs); err != nil {
			t.Fatalf("list: %s", raw)
		}
		return runs
	}

	runs := list(alice, swarm)
	if len(runs) != 1 || runs[0]["id"] != swarm {
		t.Fatalf("alpha list: %v", runs)
	}
	origin("alpha list", runs[0], alpha)
	r, b, _ = ts.do(alice, "GET", "/api/pipeline-runs/"+swarm, "", nil)
	expectStatus(t, "alpha detail", r, b, http.StatusOK)
	origin("alpha detail", b, alpha)
	runs = list(bob, beta)
	if len(runs) != 1 || runs[0]["id"] != beta {
		t.Fatalf("beta list: %v", runs)
	}
	origin("beta list", runs[0], betaOrigin)

	_, _, missing := ts.do(alice, "GET", "/api/pipeline-runs/missing", "", nil)
	for _, tc := range []struct {
		name   string
		client *http.Client
		path   string
	}{
		{"outsider detail", bob, "/api/pipeline-runs/" + swarm},
		{"outsider graph", bob, "/api/pipeline-runs/" + swarm + "/graph"},
		{"foreign project detail", alice, "/api/pipeline-runs/" + beta},
	} {
		r, b, raw := ts.do(tc.client, "GET", tc.path, "", nil)
		expectStatus(t, tc.name, r, b, http.StatusNotFound)
		if raw != missing {
			t.Fatalf("%s: %q, want the missing-run body %q", tc.name, raw, missing)
		}
	}
	r, b, _ = ts.do(alice, "GET", "/api/swarms/"+beta+"/pipeline-runs", "", nil)
	expectStatus(t, "foreign project list", r, b, http.StatusNotFound)
	if len(b) != 1 || b["detail"] != "swarm not found" {
		t.Fatalf("foreign project list leaked fields: %v", b)
	}

	r, b, _ = ts.do(alice, "POST", "/api/swarms/"+swarm+"/members", ts.csrf(alice), map[string]string{"username": "bob", "role": "viewer"})
	expectStatus(t, "add viewer", r, b, http.StatusNoContent)
	r, b, _ = ts.do(bob, "GET", "/api/pipeline-runs/"+swarm, "", nil)
	expectStatus(t, "viewer detail", r, b, http.StatusOK)
	origin("viewer detail", b, alpha)
	r, b, _ = ts.do(alice, "POST", "/api/pipeline-runs/"+swarm+"/nodes/ask/answer", ts.csrf(alice), map[string]any{"answer": "success", "visitSequence": 1})
	expectStatus(t, "answer", r, b, http.StatusOK)
	origin("answer", b, alpha)
}

// The cross-swarm graph reuses a managed UID of the first swarm: an administrator
// sees both graphs, but the name resolves only within the step's own swarm.
func TestPipelineNodeActorNameWire(t *testing.T) {
	ts, alice, master, _ := masterSetup(t)
	ctx := context.Background()
	s := ts.srv.store
	sw, err := s.GetSwarm(ctx, master.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSwarm(ctx, store.Swarm{ID: "foreign", ProjectID: sw.ProjectID, OwnerID: sw.OwnerID, Name: "foreign", AtespaceName: "foreign", AtespaceUID: strPtr("foreign-namespace"), State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSwarm(ctx, store.Swarm{ID: "native", ProjectID: sw.ProjectID, OwnerID: sw.OwnerID, Name: "native", AtespaceName: "native", AtespaceUID: strPtr("native-namespace"), State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := insertSwarmGraph(ctx, ts.testServer, master.SwarmID, []byte(actorVisitGraph), "running"); err != nil {
		t.Fatal(err)
	}
	if err := insertSwarmGraph(ctx, ts.testServer, "native", []byte(actorVisitGraph), "running"); err != nil {
		t.Fatal(err)
	}
	if err := insertSwarmGraph(ctx, ts.testServer, "foreign", []byte(actorVisitGraph), "running"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{master.SwarmID, "foreign"} {
		v, err := s.AppendPipelineVisit(ctx, id, id, store.PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: master.UID, ActorIncarnation: master.Incarnation, RequestID: "req"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CompletePipelineActorVisit(ctx, id, id, v.Sequence, "req", master.UID, master.Incarnation, "success"); err != nil {
			t.Fatal(err)
		}
	}
	native, err := s.ReservePipelineNativeActor(ctx, store.PipelineNativeReservation{SwarmID: "native", RunID: "native", NodeID: "actor", RequestID: "req", Sequence: 1, TemplateName: "development", TemplateUID: "template-uid", TemplateDigest: "sha256:" + strings.Repeat("cd", 32)})
	if err != nil {
		t.Fatal(err)
	}
	actors := func(nodes any) string {
		t.Helper()
		list, ok := nodes.([]any)
		if !ok {
			t.Fatalf("nodes: %v", nodes)
		}
		out := []string{}
		for _, n := range list {
			node := n.(map[string]any)
			name, present := node["actorName"]
			if present && (name == "" || name == nil) {
				t.Fatalf("unknown actor must be omitted, not empty: %v", node)
			}
			out = append(out, node["id"].(string)+"="+func() string { s, _ := name.(string); return s }())
		}
		return strings.Join(out, " ")
	}
	for _, tc := range []struct{ id, want string }{
		{master.SwarmID, "actor=" + master.Name + " done="},
		{"native", "actor=" + native.ActorName},
		{"foreign", "actor= done="},
	} {
		r, b, _ := ts.do(alice, "GET", "/api/pipeline-runs/"+tc.id, "", nil)
		expectStatus(t, tc.id, r, b, http.StatusOK)
		if got := actors(b["nodes"]); got != tc.want {
			t.Fatalf("%s actors: %q, want %q", tc.id, got, tc.want)
		}
	}
	r, b, raw := ts.do(alice, "GET", "/api/swarms/"+master.SwarmID+"/pipeline-runs", "", nil)
	expectStatus(t, "list", r, b, http.StatusOK)
	var runs []map[string]any
	if err := json.Unmarshal([]byte(raw), &runs); err != nil || len(runs) != 3 {
		t.Fatalf("list: %s", raw)
	}
	// Рои идут в порядке создания: master, foreign, native.
	if got := actors(runs[0]["nodes"]) + " | " + actors(runs[1]["nodes"]) + " | " + actors(runs[2]["nodes"]); got != "actor="+master.Name+" done= | actor= done= | actor="+native.ActorName {
		t.Fatalf("listed actors: %q", got)
	}
}
