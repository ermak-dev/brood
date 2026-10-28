package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

// BUG: HTTP project deletion bypasses native swarm deletion and loses bindings (#67).
// HYPOTHESIS: The handler uses the cascading primitive instead of the guarded operation.
// SOLUTION: Revalidate the administrator and use the safe delete in one write transaction.
// RESULT: FIXED — live bindings survive refusal; empty and properly deleted projects succeed.
func TestHTTPProjectDeletionRefusesLiveSwarms(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "password")
	admin := newClient(t)
	ts.login(admin, "root", "password")
	r, body, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]string{"username": "member", "password": "password"})
	expectStatus(t, "user", r, body, 201)
	member := newClient(t)
	ts.login(member, "member", "password")
	for _, state := range []string{"provisioning", "active", "error", "deleting"} {
		t.Run(state, func(t *testing.T) {
			r, project, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "guard-" + state})
			expectStatus(t, "project", r, project, 201)
			projectID := project["id"].(string)
			prefix := "/api/projects/" + projectID
			r, body, _ := ts.do(admin, "POST", prefix+"/members", ts.csrf(admin), map[string]string{"username": "member"})
			expectStatus(t, "membership", r, body, 204)
			r, swarm, _ := ts.do(member, "POST", prefix+"/swarms", ts.csrf(member), map[string]string{"name": "sw"})
			expectStatus(t, "swarm", r, swarm, 201)
			swarmID := swarm["id"].(string)
			if err := ts.srv.store.SetSwarmState(context.Background(), swarmID, state); err != nil {
				t.Fatal(err)
			}
			r, body, _ = ts.do(admin, "DELETE", prefix, ts.csrf(admin), nil)
			expectStatus(t, "guarded project deletion", r, body, http.StatusConflict)
			if detail, _ := body["detail"].(string); !strings.Contains(detail, "swarms") {
				t.Fatalf("refusal lacks a useful cause: %v", body)
			}
			if _, err := ts.srv.store.GetProject(context.Background(), projectID); err != nil {
				t.Fatalf("refusal lost project: %v", err)
			}
			r, body, _ = ts.do(member, "GET", "/api/swarms/"+swarmID, "", nil)
			expectStatus(t, "swarm remains accessible", r, body, 200)
		})
	}
}

func TestHTTPProjectDeletionStillRequiresAdminAndCSRF(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "password")
	admin := newClient(t)
	ts.login(admin, "root", "password")
	r, body, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]string{"username": "member", "password": "password"})
	expectStatus(t, "user", r, body, 201)
	member := newClient(t)
	ts.login(member, "member", "password")
	r, project, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "empty"})
	expectStatus(t, "project", r, project, 201)
	url := "/api/projects/" + project["id"].(string)
	r, body, _ = ts.do(member, "DELETE", url, ts.csrf(member), nil)
	expectStatus(t, "member denied", r, body, 403)
	anonymous := newClient(t)
	ts.do(anonymous, "GET", "/api/auth/session", "", nil)
	r, body, _ = ts.do(anonymous, "DELETE", url, ts.csrf(anonymous), nil)
	expectStatus(t, "anonymous denied", r, body, 401)
	r, body, _ = ts.do(admin, "DELETE", url, "", nil)
	expectStatus(t, "csrf denied", r, body, 403)
	r, body, _ = ts.do(admin, "DELETE", url, ts.csrf(admin), nil)
	expectStatus(t, "empty project deleted", r, body, 204)
	if _, err := ts.srv.store.GetProject(context.Background(), project["id"].(string)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty project remains: %v", err)
	}
	r, body, _ = ts.do(admin, "DELETE", url, ts.csrf(admin), nil)
	expectStatus(t, "missing project", r, body, 404)
}

func TestHTTPProjectDeletionAfterNativeSwarmDeletion(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "password")
	admin := newClient(t)
	ts.login(admin, "root", "password")
	r, project, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "finished"})
	expectStatus(t, "project", r, project, 201)
	prefix := "/api/projects/" + project["id"].(string)
	r, swarm, _ := ts.do(admin, "POST", prefix+"/swarms", ts.csrf(admin), map[string]string{"name": "sw"})
	expectStatus(t, "swarm", r, swarm, 201)
	r, body, _ := ts.do(admin, "DELETE", "/api/swarms/"+swarm["id"].(string), ts.csrf(admin), nil)
	expectStatus(t, "native deletion", r, body, 204)
	r, body, _ = ts.do(admin, "DELETE", prefix, ts.csrf(admin), nil)
	expectStatus(t, "tombstone project deletion", r, body, 204)
}
