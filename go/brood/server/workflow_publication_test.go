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
