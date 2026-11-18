package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

func TestWorkflowPolicyAPIReadsVersionAndAppendsWithExactCAS(t *testing.T) {
	ts, admin, member, outsider, project := gitRepositorySetup(t)
	path := "/api/projects/" + project + "/workflow-policy"
	r, body, _ := ts.do(member, "GET", path, "", nil)
	expectStatus(t, "implicit empty policy", r, body, http.StatusOK)
	want := map[string]any{"projectId": project, "revision": float64(0), "rules": []any{}, "createdBy": "", "createdAt": ""}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("implicit default lost explicit revision or rules: %v", body)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("policy response may be cached")
	}
	r, body, _ = ts.do(outsider, "GET", path, "", nil)
	expectStatus(t, "outside policy read", r, body, http.StatusNotFound)
	rule := map[string]any{"id": "storage", "paths": []string{"go/**"}, "require": "human"}
	r, body, _ = ts.do(admin, "PUT", path, ts.csrf(admin), map[string]any{"revision": 0, "rules": []any{rule}})
	expectStatus(t, "first policy publication", r, body, http.StatusOK)
	if body["revision"] != float64(1) || body["projectId"] != project || body["createdBy"] == "" || body["createdAt"] == "" {
		t.Fatalf("missing policy identity: %v", body)
	}
	first, err := ts.srv.store.ProjectWorkflowPolicy(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	r, body, _ = ts.do(member, "GET", path, "", nil)
	expectStatus(t, "member reads configured policy", r, body, http.StatusOK)
	if body["revision"] != float64(1) || len(body["rules"].([]any)) != 1 {
		t.Fatal("member got empty or stale policy")
	}
	r, body, _ = ts.do(admin, "PUT", path, ts.csrf(admin), map[string]any{"revision": 0, "rules": []any{}})
	expectStatus(t, "lost update blocked", r, body, http.StatusConflict)
	current, err := ts.srv.store.ProjectWorkflowPolicy(context.Background(), project)
	if err != nil || !reflect.DeepEqual(current, first) {
		t.Fatal("stale CAS changed current policy")
	}
	r, body, _ = ts.do(admin, "PUT", path, ts.csrf(admin), map[string]any{"revision": 1, "rules": []any{}})
	expectStatus(t, "explicit empty policy publication", r, body, http.StatusOK)
	if body["revision"] != float64(2) || !reflect.DeepEqual(body["rules"], []any{}) {
		t.Fatal("clearing rules erased version history")
	}
	historical, err := ts.srv.store.ProjectWorkflowPolicyVersion(context.Background(), project, 1)
	if err != nil || !reflect.DeepEqual(historical, first) {
		t.Fatal("policy update rewrote prior version")
	}
}

func TestWorkflowPolicyAPIRejectsAmbiguousBodiesAndInvalidRules(t *testing.T) {
	ts, admin, _, _, project := gitRepositorySetup(t)
	path := "/api/projects/" + project + "/workflow-policy"
	bodies := []string{
		`{}`, `{"revision":0}`, `{"rules":[]}`, `{"revision":null,"rules":[]}`, `{"revision":0,"rules":null}`,
		`{"revision":"0","rules":[]}`, `{"revision":0.5,"rules":[]}`, `{"Revision":0,"rules":[]}`,
		`{"revision":0,"revision":1,"rules":[]}`, `{"revision":0,"rules":[],"extra":true}`,
		`{"revision":0,"rules":[]} {}`, `{"revision":0,"rules":[{"id":"storage","ID":"other","paths":["go/**"],"require":"human"}]}`,
		`{"revision":0,"rules":[{"id":"storage","paths":["go/**"],"require":"human","require":"deny"}]}`,
		`{"revision":0,"rules":[{"id":"storage","paths":["go/**"],"require":"human","approved":true}]}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			req, err := http.NewRequest("PUT", ts.http.URL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Origin", ts.http.URL)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(auth.HeaderCSRF, ts.csrf(admin))
			r, err := admin.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			var b map[string]any
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
				t.Fatal(err)
			}
			expectStatus(t, "strict policy JSON", r, b, http.StatusBadRequest)
		})
	}
	for _, rule := range []map[string]any{
		{"id": "storage", "paths": []string{"../secret"}, "require": "human"},
		{"id": "storage", "paths": []string{"go/**"}, "require": "maybe"},
		{"id": "storage", "actions": []string{"publish"}, "require": "human"},
		{"id": "storage", "paths": []string{"go/**"}, "require": "independent_review"},
	} {
		r, b, _ := ts.do(admin, "PUT", path, ts.csrf(admin), map[string]any{"revision": 0, "rules": []any{rule}})
		expectStatus(t, "invalid policy rule", r, b, http.StatusUnprocessableEntity)
	}
	current, err := ts.srv.store.ProjectWorkflowPolicy(context.Background(), project)
	if err != nil || current.Revision != 0 || len(current.Rules) != 0 {
		t.Fatal("refused request changed policy")
	}
}

func TestWorkflowPolicyAPIRejectsOversizedStoredRulesAsClientError(t *testing.T) {
	ts, admin, _, _, project := gitRepositorySetup(t)
	var rules []map[string]any
	paths := make([]string, 32)
	for i := range paths {
		paths[i] = strings.Repeat("x", 100)
	}
	for i := range 64 {
		rules = append(rules, map[string]any{"id": fmt.Sprintf("rule%d", i), "paths": paths, "require": "human"})
	}
	r, body, _ := ts.do(admin, "PUT", "/api/projects/"+project+"/workflow-policy", ts.csrf(admin), map[string]any{"revision": 0, "rules": rules})
	expectStatus(t, "bounded stored policy", r, body, http.StatusUnprocessableEntity)
	current, err := ts.srv.store.ProjectWorkflowPolicy(context.Background(), project)
	if err != nil || current.Revision != 0 {
		t.Fatal("oversized policy changed default")
	}
}

func TestWorkflowPolicyAPIRequiresCurrentAdminSessionForWrites(t *testing.T) {
	ts, admin, member, outsider, project := gitRepositorySetup(t)
	path := "/api/projects/" + project + "/workflow-policy"
	for _, client := range []*http.Client{member, outsider} {
		r, b, _ := ts.do(client, "PUT", path, ts.csrf(client), map[string]any{"revision": 0, "rules": []any{}})
		expectStatus(t, "policy mutation requires admin", r, b, http.StatusForbidden)
	}
	r, b, _ := ts.do(admin, "PUT", path, "", map[string]any{"revision": 0, "rules": []any{}})
	expectStatus(t, "policy mutation requires CSRF", r, b, http.StatusForbidden)
	r, b, _ = ts.do(newClient(t), "GET", path, "", nil)
	expectStatus(t, "policy read requires session", r, b, http.StatusUnauthorized)
	r, b, _ = ts.do(admin, "GET", "/api/projects/missing/workflow-policy", "", nil)
	expectStatus(t, "missing policy project", r, b, http.StatusNotFound)
}

func TestWorkflowPolicyAPIRechecksAuthorityAfterBodyRead(t *testing.T) {
	for _, change := range []string{"revoked", "disabled", "demoted"} {
		t.Run(change, func(t *testing.T) {
			ts, admin, _, _, project := gitRepositorySetup(t)
			ctx := context.Background()
			if err := ts.srv.store.CreateUser(ctx, store.User{ID: "policy-spare-admin", Username: "policy-spare-admin", PasswordHash: "test-only", IsAdmin: true}); err != nil {
				t.Fatal(err)
			}
			user, err := ts.srv.store.GetUserByUsername(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			read := false
			reader := &projectSecretAuthorityReader{Reader: bytes.NewReader([]byte(`{"revision":0,"rules":[]}`)), beforeRead: func() {
				read = true
				var err error
				switch change {
				case "revoked":
					_, err = auth.NewSessions(ts.srv.store, 0).RevokeAllOfUser(ctx, user.ID)
				case "disabled":
					err = ts.srv.store.SetUserDisabled(ctx, user.ID, true)
				case "demoted":
					err = ts.srv.store.SetUserAdmin(ctx, user.ID, false)
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			req := httptest.NewRequest("PUT", ts.http.URL+"/api/projects/"+project+"/workflow-policy", reader)
			req.Header.Set("Origin", ts.http.URL)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(auth.HeaderCSRF, ts.csrf(admin))
			for _, cookie := range admin.Jar.Cookies(req.URL) {
				req.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			ts.srv.Handler().ServeHTTP(w, req)
			if !read || w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "authorization changed") {
				t.Fatalf("late %s: read=%v status=%d body=%s", change, read, w.Code, w.Body.String())
			}
			current, err := ts.srv.store.ProjectWorkflowPolicy(ctx, project)
			if err != nil || current.Revision != 0 {
				t.Fatal("late authority loss changed policy")
			}
		})
	}
}

func TestWorkflowPolicyAPIUpdateCannotReplacePinnedPassPolicy(t *testing.T) {
	f := newResultCollectionFixtureWithGraph(t, false, policyExitGraph)
	ctx := context.Background()
	swarm, err := f.srv.store.GetSwarm(ctx, f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.srv.store.SwarmStepWorkflowPolicy(ctx, f.swarm, 1)
	if err != nil {
		t.Fatal(err)
	}
	admin := newClient(t)
	f.login(admin, "root", "root-password-1")
	path := "/api/projects/" + swarm.ProjectID + "/workflow-policy"
	r, b, _ := f.do(admin, "PUT", path, f.csrf(admin), map[string]any{"revision": 1, "rules": []any{map[string]any{"id": "deny-work", "actions": []string{"work"}, "require": "deny"}}})
	expectStatus(t, "new default during pass", r, b, http.StatusOK)
	after, err := f.srv.store.SwarmStepWorkflowPolicy(ctx, f.swarm, 1)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("policy API rewrote active pass snapshot")
	}
	accepted := f.completion(t)
	if !accepted.OK || !f.acknowledge(t, accepted).OK {
		t.Fatal("pinned pass completion rejected")
	}
	f.srv.steps.wait()
	candidate, err := f.srv.store.GetPipelinePolicyCandidate(ctx, f.swarm, 1, "result")
	if err != nil || candidate.Preview.Policy.Revision != 1 || candidate.Preview.Decision.State != "waiting_human" {
		t.Fatal("new API default replaced policy at result boundary")
	}
}
