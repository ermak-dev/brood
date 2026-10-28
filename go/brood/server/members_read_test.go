package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestProjectMembershipListing(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "password")
	admin := newClient(t)
	ts.login(admin, "root", "password")
	response, project, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "test"})
	expectStatus(t, "create", response, project, http.StatusCreated)
	path := "/api/projects/" + project["id"].(string) + "/members"
	response, _, raw := ts.do(admin, "GET", path, "", nil)
	if response.StatusCode != 200 || strings.TrimSpace(raw) != "[]" {
		t.Fatalf("empty members: %d %s", response.StatusCode, raw)
	}
	ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]any{"username": "alice", "password": "password"})
	response, _, _ = ts.do(admin, "POST", path, ts.csrf(admin), map[string]string{"username": "alice"})
	expectStatus(t, "add", response, nil, 204)
	response, _, raw = ts.do(admin, "GET", path, "", nil)
	var members []map[string]any
	if err := json.Unmarshal([]byte(raw), &members); err != nil || response.StatusCode != 200 || len(members) != 1 || members[0]["username"] != "alice" {
		t.Fatalf("members response: %d %s %v", response.StatusCode, raw, err)
	}
	if strings.Contains(raw, "password") || strings.Contains(raw, "argon2") {
		t.Fatal("members response leaked credentials")
	}
	alice := newClient(t)
	ts.login(alice, "alice", "password")
	response, _, _ = ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "project member choices", response, nil, 200)
	response, _, _ = ts.do(newClient(t), "GET", path, "", nil)
	expectStatus(t, "anonymous members access", response, nil, 401)
	response, _, _ = ts.do(admin, "GET", "/api/projects/missing/members", "", nil)
	expectStatus(t, "missing project", response, nil, 404)
}
