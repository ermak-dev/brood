package server

// FEATURE: Project workflow publication.
// SPEC: Authorize every request, reject ambiguous or invalid batches, and publish atomically.
// IMPLEMENTATION: Validate requests before the transaction and recheck write authority inside it.
// RESULT: IMPLEMENTED; workflow publication tests pass.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/pipeline"
)

func workflowDocument(name string) string {
	return strings.Replace(pipelineAPIGraph, "name: test", "name: "+name, 1)
}

func workflowRaw(t *testing.T, ts *testServer, client *http.Client, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, ts.http.URL+path, strings.NewReader(body))
	req.Header.Set("Origin", ts.http.URL)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.HeaderCSRF, ts.csrf(client))
	for _, cookie := range client.Jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	return w
}

func workflowRead(t *testing.T, ts *testServer, client *http.Client, path string) map[string]any {
	t.Helper()
	r, body, _ := ts.do(client, "GET", path, "", nil)
	expectStatus(t, "read workflows", r, body, http.StatusOK)
	definitions, ok := body["workflows"].(map[string]any)
	if !ok {
		t.Fatalf("missing workflow definitions: %v", body)
	}
	return definitions
}

func workflowPut(t *testing.T, ts *testServer, client *http.Client, path string, definitions map[string]string) {
	t.Helper()
	r, body, _ := ts.do(client, "PUT", path, ts.csrf(client), map[string]any{"workflows": definitions})
	if r.StatusCode != http.StatusOK && r.StatusCode != http.StatusNoContent {
		t.Fatalf("publish workflows: status %d, want 200 or 204: %v", r.StatusCode, body)
	}
}

// FEATURE: Explicit workflow deletion.
// SPEC: Deletion is admin-only and does not happen during publication.
// IMPLEMENTATION: Add a separate named deletion endpoint.
// RESULT: IMPLEMENTED — deletion is explicit and authorized.
// STATUS: DONE
func TestWorkflowPublicationDelete(t *testing.T) {
	ts, admin, member, outsider, id := gitRepositorySetup(t)
	path := "/api/projects/" + id + "/workflows"
	workflowPut(t, ts, admin, path, map[string]string{"test": workflowDocument("test")})
	for _, client := range []*http.Client{member, outsider} {
		r, _, _ := ts.do(client, "DELETE", path+"/test", ts.csrf(client), nil)
		if r.StatusCode != http.StatusForbidden && r.StatusCode != http.StatusNotFound {
			t.Fatalf("unauthorized delete: %d", r.StatusCode)
		}
	}
	r, _, _ := ts.do(admin, "DELETE", path+"/test", ts.csrf(admin), nil)
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("admin delete: %d", r.StatusCode)
	}
	if len(workflowRead(t, ts, admin, path)) != 0 {
		t.Fatal("definition remains after deletion")
	}
}

// FEATURE: Project workflow catalog and definition graph.
// SPEC: Preserve the existing snapshot, expose version and publication time, and enforce project visibility.
// IMPLEMENTATION: Add scoped catalog and graph routes and nullable publication timestamps.
// RESULT: IMPLEMENTED — existing snapshot and legacy timestamps are preserved.
// STATUS: DONE
func TestWorkflowGraphRevisionRequired(t *testing.T) {
	ts, admin, member, _, id := gitRepositorySetup(t)
	path := "/api/projects/" + id + "/workflows"
	workflowPut(t, ts, admin, path, map[string]string{"test": workflowDocument("test")})
	r, body, _ := ts.do(member, "GET", path+"/test/graph?version=1", "", nil)
	if r.StatusCode != http.StatusBadRequest || body["svg"] != nil {
		t.Fatalf("missing revision: status %d, body %v", r.StatusCode, body)
	}
}

func TestWorkflowGraphVersion(t *testing.T) {
	ts, admin, member, outsider, id := gitRepositorySetup(t)
	path := "/api/projects/" + id + "/workflows"
	graph := path + "/test/graph"
	doc := workflowDocument("test")
	workflowPut(t, ts, admin, path, map[string]string{"test": doc})
	r, body, _ := ts.do(member, "GET", "/api/projects/"+id+"/workflow-catalog", "", nil)
	expectStatus(t, "catalog", r, body, http.StatusOK)
	item := body["workflows"].([]any)[0].(map[string]any)
	if item["version"] != float64(1) {
		t.Fatalf("unexpected catalog: %v", body)
	}
	revision, ok := item["revision"].(string)
	if !ok || len(revision) != 32 {
		t.Fatalf("missing revision: %v", item)
	}
	workflowPut(t, ts, admin, path, map[string]string{"test": strings.Replace(doc, "Continue?", "Proceed?", 1)})
	r, body, _ = ts.do(member, "GET", graph+"?version=1&revision="+revision, "", nil)
	if r.StatusCode != http.StatusConflict || body["svg"] != nil {
		t.Fatalf("stale graph: status %d, body %v", r.StatusCode, body)
	}
	r, body, _ = ts.do(member, "GET", "/api/projects/"+id+"/workflow-catalog", "", nil)
	expectStatus(t, "updated catalog", r, body, http.StatusOK)
	updatedRevision := body["workflows"].([]any)[0].(map[string]any)["revision"].(string)
	r, body, _ = ts.do(member, "GET", graph+"?version=2&revision="+revision, "", nil)
	if r.StatusCode != http.StatusConflict || body["svg"] != nil {
		t.Fatalf("wrong revision: status %d, body %v", r.StatusCode, body)
	}
	r, body, _ = ts.do(member, "GET", graph+"?version=2&revision="+updatedRevision, "", nil)
	expectStatus(t, "current graph", r, body, http.StatusOK)
	if !strings.Contains(body["svg"].(string), "<svg") {
		t.Fatal("missing graph")
	}
	r, body, _ = ts.do(member, "GET", graph, "", nil)
	expectStatus(t, "legacy graph", r, body, http.StatusOK)
	r, _, _ = ts.do(outsider, "GET", graph+"?version=1", "", nil)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider: %d", r.StatusCode)
	}
	for _, query := range []string{"?version=", "?version=0", "?version=x", "?version=1&version=2"} {
		r, _, _ = ts.do(member, "GET", graph+query, "", nil)
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("invalid %s: %d", query, r.StatusCode)
		}
	}
}

func TestWorkflowRevisionChangesAfterIdenticalRecreate(t *testing.T) {
	ts, admin, member, _, id := gitRepositorySetup(t)
	path := "/api/projects/" + id + "/workflows"
	catalog := "/api/projects/" + id + "/workflow-catalog"
	doc := workflowDocument("test")
	workflowPut(t, ts, admin, path, map[string]string{"test": doc})
	r, body, _ := ts.do(member, "GET", catalog, "", nil)
	expectStatus(t, "catalog", r, body, http.StatusOK)
	first := body["workflows"].([]any)[0].(map[string]any)["revision"].(string)
	r, body, _ = ts.do(admin, "DELETE", path+"/test", ts.csrf(admin), nil)
	expectStatus(t, "delete", r, body, http.StatusNoContent)
	workflowPut(t, ts, admin, path, map[string]string{"test": doc})
	r, body, _ = ts.do(member, "GET", catalog, "", nil)
	expectStatus(t, "catalog after recreate", r, body, http.StatusOK)
	second := body["workflows"].([]any)[0].(map[string]any)["revision"].(string)
	if first == second {
		t.Fatal("identical recreated workflow reused its revision identity")
	}
}

func TestWorkflowGraphVersionAfterRecreate(t *testing.T) {
	ts, admin, member, _, id := gitRepositorySetup(t)
	path := "/api/projects/" + id + "/workflows"
	doc := workflowDocument("test")
	workflowPut(t, ts, admin, path, map[string]string{"test": doc})
	r, body, _ := ts.do(member, "GET", "/api/projects/"+id+"/workflow-catalog", "", nil)
	expectStatus(t, "catalog", r, body, http.StatusOK)
	item := body["workflows"].([]any)[0].(map[string]any)
	if item["version"] != float64(1) {
		t.Fatalf("unexpected catalog: %v", body)
	}
	revision := item["revision"].(string)
	r, body, _ = ts.do(admin, "DELETE", path+"/test", ts.csrf(admin), nil)
	expectStatus(t, "delete", r, body, http.StatusNoContent)
	workflowPut(t, ts, admin, path, map[string]string{"test": strings.Replace(doc, "Continue?", "Proceed?", 1)})
	r, body, _ = ts.do(member, "GET", path+"/test/graph?version=1&revision="+revision, "", nil)
	if r.StatusCode != http.StatusConflict || body["svg"] != nil {
		t.Fatalf("stale version accepted after recreation: status %d, body %v", r.StatusCode, body)
	}
}

func TestWorkflowCatalog(t *testing.T) {
	ts, admin, member, outsider, id := gitRepositorySetup(t)
	path := "/api/projects/" + id + "/workflows"
	catalog := "/api/projects/" + id + "/workflow-catalog"
	graph := path + "/test/graph"
	doc := workflowDocument("test")
	workflowPut(t, ts, admin, path, map[string]string{"test": doc})
	if got := workflowRead(t, ts, member, path); !reflect.DeepEqual(got, map[string]any{"test": doc}) {
		t.Fatalf("legacy snapshot changed: %v", got)
	}
	r, body, _ := ts.do(member, "GET", catalog, "", nil)
	expectStatus(t, "catalog", r, body, http.StatusOK)
	items, ok := body["workflows"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("catalog: %v", body)
	}
	item := items[0].(map[string]any)
	if item["name"] != "test" || item["version"] != float64(1) || item["publishedAt"] == nil {
		t.Fatalf("catalog entry: %v", item)
	}
	timestamp := item["publishedAt"]
	workflowPut(t, ts, admin, path, map[string]string{"test": doc})
	_, body, _ = ts.do(member, "GET", catalog, "", nil)
	if body["workflows"].([]any)[0].(map[string]any)["publishedAt"] != timestamp {
		t.Fatal("idempotent sync changed timestamp")
	}
	if _, err := ts.srv.catalog.db.Exec(`UPDATE workflow_definitions SET published_at=NULL WHERE project_id=? AND name='test'`, id); err != nil {
		t.Fatal(err)
	}
	_, body, _ = ts.do(member, "GET", catalog, "", nil)
	item = body["workflows"].([]any)[0].(map[string]any)
	if value, exists := item["publishedAt"]; !exists || value != nil {
		t.Fatalf("unknown timestamp: %v", item)
	}
	workflowPut(t, ts, admin, path, map[string]string{"test": doc + "\n"})
	_, body, _ = ts.do(member, "GET", catalog, "", nil)
	item = body["workflows"].([]any)[0].(map[string]any)
	if item["version"] != float64(2) || item["publishedAt"] == nil {
		t.Fatalf("updated catalog entry: %v", item)
	}
	r, body, _ = ts.do(member, "GET", graph, "", nil)
	expectStatus(t, "definition graph", r, body, http.StatusOK)
	svg, ok := body["svg"].(string)
	// Определение без прогона не подсвечивает узлы: у групп узлов нет
	// активного класса (правило .state-active во встроенном стиле —
	// не состояние узла).
	if !ok || !strings.Contains(svg, "<svg") || !strings.Contains(svg, ">ask</text>") || !strings.Contains(svg, ">done</text>") || strings.Contains(svg, `state-active">`) {
		t.Fatalf("definition graph: %v", body)
	}
	for _, p := range []string{catalog, graph} {
		r, _, _ = ts.do(outsider, "GET", p, "", nil)
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("outsider %s: %d", p, r.StatusCode)
		}
	}
	r, _, _ = ts.do(member, "GET", path+"/missing/graph", "", nil)
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("missing workflow: %d", r.StatusCode)
	}
}

func TestWorkflowPublicationLifecycle(t *testing.T) {
	graph := workflowDocument("test")
	if _, err := pipeline.Parse(graph, "test.yaml"); err != nil {
		t.Fatalf("fixture graph: %v", err)
	}
	ts, admin, member, outsider, id := gitRepositorySetup(t)
	path := "/api/projects/" + id + "/workflows"
	workflowPut(t, ts, admin, path, map[string]string{"test": graph})
	want := map[string]any{"test": graph}
	if got := workflowRead(t, ts, member, path); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	workflowPut(t, ts, member, path, map[string]string{"test": graph})
	workflowPut(t, ts, admin, path, map[string]string{})
	second := workflowDocument("second")
	workflowPut(t, ts, admin, path, map[string]string{"second": second})
	want["second"] = second
	if got := workflowRead(t, ts, admin, path); !reflect.DeepEqual(got, want) {
		t.Fatalf("repeat, empty map or partial update changed existing definitions: %v", got)
	}
	changed := strings.Replace(graph, "Continue?", "Proceed?", 1)
	r, _, _ := ts.do(member, "PUT", path, ts.csrf(member), map[string]any{"workflows": map[string]string{"test": changed}})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("member change: %d, want 403", r.StatusCode)
	}
	for _, method := range []string{"GET", "PUT"} {
		r, _, raw := ts.do(outsider, method, path, ts.csrf(outsider), map[string]any{"workflows": map[string]string{"test": graph}})
		if r.StatusCode != http.StatusForbidden && r.StatusCode != http.StatusNotFound {
			t.Errorf("outsider %s: %d", method, r.StatusCode)
		}
		if strings.Contains(raw, graph) || strings.Contains(raw, "Continue?") {
			t.Errorf("outsider %s observed definition: %s", method, raw)
		}
	}
	if got := workflowRead(t, ts, admin, path); !reflect.DeepEqual(got, want) {
		t.Fatalf("unauthorized change modified definitions: %v", got)
	}
}

func TestWorkflowPublicationRevokedDuringRead(t *testing.T) {
	ts, admin, _, _, id := gitRepositorySetup(t)
	ctx := context.Background()
	user, err := ts.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	reader := &projectSecretAuthorityReader{Reader: strings.NewReader(`{"workflows":{}}`), beforeRead: func() {
		if _, err := auth.NewSessions(ts.srv.store, 0).RevokeAllOfUser(ctx, user.ID); err != nil {
			t.Fatal(err)
		}
	}}
	req := httptest.NewRequest("PUT", ts.http.URL+"/api/projects/"+id+"/workflows", reader)
	req.Header.Set("Origin", ts.http.URL)
	req.Header.Set(auth.HeaderCSRF, ts.csrf(admin))
	for _, c := range admin.Jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoked: %d %s", w.Code, w.Body.String())
	}
}

func TestWorkflowPublicationEscapedProject(t *testing.T) {
	ts, admin, _, _, _ := gitRepositorySetup(t)
	w := workflowRaw(t, ts, admin, "/api/projects/not%2Fproject/workflows", `{"workflows":{}}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("escaped project: %d %s", w.Code, w.Body.String())
	}
}

func TestWorkflowPublicationRejectsAtomically(t *testing.T) {
	ts, admin, _, _, id := gitRepositorySetup(t)
	path := "/api/projects/" + id + "/workflows"
	original := workflowDocument("test")
	workflowPut(t, ts, admin, path, map[string]string{"test": original})
	valid := strings.Replace(original, "Continue?", "Changed?", 1)
	encode := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cases := map[string]string{
		"invalid graph":       encode(map[string]any{"workflows": map[string]string{"test": valid, "bad": "not a pipeline"}}),
		"name mismatch":       encode(map[string]any{"workflows": map[string]string{"test": valid, "wrong": workflowDocument("second")}}),
		"unsupported version": encode(map[string]any{"workflows": map[string]string{"test": valid, "second": strings.Replace(workflowDocument("second"), "brood.dev/v1alpha1", "brood.dev/v999", 1)}}),
		"duplicate keys":      `{"workflows":{"test":` + encode(valid) + `,"test":` + encode(original) + `}}`,
		"duplicate root":      `{"workflows":{},"workflows":{}}`,
		"null map":            `{"workflows":null}`,
		"unknown field":       `{"workflows":{},"other":true}`,
		"oversized":           encode(map[string]any{"workflows": map[string]string{"test": valid, "second": strings.Repeat("x", 2<<20)}}),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := workflowRaw(t, ts, admin, path, body)
			if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("invalid request: status %d, body %s", w.Code, w.Body.String())
			}
			if got := workflowRead(t, ts, admin, path); !reflect.DeepEqual(got, map[string]any{"test": original}) {
				t.Fatalf("invalid request modified definitions: %v", got)
			}
		})
	}
}
