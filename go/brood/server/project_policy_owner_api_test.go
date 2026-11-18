package server

import (
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
)

// FEATURE: Редактирование политики требует явного владельца проекта или admin.
// SPEC: Старые members не получают права по отсутствию owner; назначает admin,
// membership/disabled/session перепроверяются при записи, owner не становится admin.
func TestExplicitProjectOwnerCanEditOnlyTheirProjectPolicy(t *testing.T) {
	ts, admin, alice, outsider, project := gitRepositorySetup(t)
	policyPath := "/api/projects/" + project + "/workflow-policy"
	ownerPath := "/api/projects/" + project + "/owner"
	body := map[string]any{"revision": 0, "rules": []any{}}
	r, b, _ := ts.do(alice, "PUT", policyPath, ts.csrf(alice), body)
	expectStatus(t, "ordinary member cannot edit policy", r, b, http.StatusForbidden)
	r, b, _ = ts.do(admin, "PUT", ownerPath, ts.csrf(admin), map[string]any{"revision": 0, "username": "alice"})
	expectStatus(t, "explicit owner assignment", r, b, http.StatusOK)
	if b["revision"] != float64(1) {
		t.Fatalf("owner assignment needs durable revision: %+v", b)
	}
	r, b, _ = ts.do(alice, "PUT", ownerPath, ts.csrf(alice), map[string]any{"revision": 1, "username": "carol"})
	expectStatus(t, "policy owner cannot assign ownership", r, b, http.StatusForbidden)
	r, b, _ = ts.do(alice, "PUT", policyPath, ts.csrf(alice), body)
	expectStatus(t, "explicit project owner edits policy", r, b, http.StatusOK)
	if b["revision"] != float64(1) {
		t.Fatal("owner policy edit did not retain a version")
	}
	r, b, _ = ts.do(outsider, "PUT", policyPath, ts.csrf(outsider), map[string]any{"revision": 1, "rules": []any{}})
	expectStatus(t, "foreign user cannot use project owner permission", r, b, http.StatusForbidden)
	r, b, _ = ts.do(admin, "PUT", ownerPath, ts.csrf(admin), map[string]any{"revision": 1, "username": ""})
	expectStatus(t, "explicit owner revocation", r, b, http.StatusOK)
	r, b, _ = ts.do(alice, "PUT", policyPath, ts.csrf(alice), map[string]any{"revision": 1, "rules": []any{}})
	expectStatus(t, "revoked owner cannot edit policy", r, b, http.StatusForbidden)
	r, b, _ = ts.do(alice, "POST", "/api/projects", ts.csrf(alice), map[string]string{"name": "must-not-be-admin"})
	expectStatus(t, "project policy ownership does not grant installation admin", r, b, http.StatusForbidden)
}

func TestProjectOwnerAPIRefusesAmbiguousOrOversizedMutation(t *testing.T) {
	ts, admin, _, _, project := gitRepositorySetup(t)
	path := "/api/projects/" + project + "/owner"
	for _, body := range []string{
		`{}`, `{"revision":0}`, `{"username":"alice"}`,
		`{"revision":null,"username":"alice"}`, `{"revision":"0","username":"alice"}`,
		`{"revision":0.5,"username":"alice"}`, `{"revision":-1,"username":"alice"}`,
		`{"revision":0,"username":null}`, `{"revision":0,"username":17}`,
		`{"revision":0,"revision":1,"username":"alice"}`,
		`{"revision":0,"username":"alice","username":"carol"}`,
		`{"Revision":0,"username":"alice"}`, `{"revision":0,"Username":"alice"}`,
		`{"revision":0,"username":"alice","ownerUid":"foreign"}`,
		`{"revision":0,"username":"alice"} {}`,
		"{\"revision\":0,\"username\":\"" + string([]byte{0xff}) + "\"}",
		`{"revision":0,"username":"` + strings.Repeat("x", 18000) + `"}`,
	} {
		request, err := http.NewRequest("PUT", ts.http.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", ts.http.URL)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(auth.HeaderCSRF, ts.csrf(admin))
		response, err := admin.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid ownership mutation accepted: status=%d", response.StatusCode)
		}
	}
}
