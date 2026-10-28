// FEATURE: Administrator-only CRUD for editable user properties.
// SPEC: Properties change atomically; the last administrator and live swarm owners are protected.
package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestAdminUserCRUD(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "password")
	admin := newClient(t)
	ts.login(admin, "root", "password")
	r, u, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]any{"username": "member", "password": "password", "display_name": "Member"})
	expectStatus(t, "create", r, u, 201)
	r, u, raw := ts.do(admin, "GET", "/api/admin/users/member", "", nil)
	expectStatus(t, "get", r, u, 200)
	if u["display_name"] != "Member" || strings.Contains(raw, "password") || strings.Contains(raw, "argon2") {
		t.Fatalf("public user: %s", raw)
	}
	member := newClient(t)
	ts.login(member, "member", "password")
	body := map[string]any{"display_name": "Edited", "is_admin": true, "disabled": true}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		r, u, _ = ts.do(member, method, "/api/admin/users/root", ts.csrf(member), body)
		expectStatus(t, "member "+method, r, u, 403)
	}
	r, u, _ = ts.do(admin, "PUT", "/api/admin/users/member", "", body)
	expectStatus(t, "csrf", r, u, 403)
	r, u, _ = ts.do(admin, "PUT", "/api/admin/users/member", ts.csrf(admin), map[string]any{"display_name": "Incomplete"})
	expectStatus(t, "incomplete", r, u, 400)
	r, u, _ = ts.do(admin, "PUT", "/api/admin/users/member", ts.csrf(admin), body)
	expectStatus(t, "update", r, u, 204)
	r, u, _ = ts.do(admin, "GET", "/api/admin/users/member", "", nil)
	expectStatus(t, "updated", r, u, 200)
	if u["display_name"] != "Edited" || u["is_admin"] != true || u["disabled"] != true {
		t.Fatalf("wrong properties: %v", u)
	}
	r, u, _ = ts.do(member, "GET", "/api/auth/session", "", nil)
	expectStatus(t, "revoked", r, u, 200)
	if u["authenticated"] != false {
		t.Fatal("edited privileges retained a session")
	}
	r, u, _ = ts.do(admin, "DELETE", "/api/admin/users/member", ts.csrf(admin), nil)
	expectStatus(t, "delete", r, u, 204)
	r, u, _ = ts.do(admin, "GET", "/api/admin/users/member", "", nil)
	expectStatus(t, "deleted", r, u, 404)
	r, u, _ = ts.do(admin, "PUT", "/api/admin/users/root", ts.csrf(admin), map[string]any{"display_name": "Must not save", "is_admin": false, "disabled": true})
	expectStatus(t, "last admin update", r, u, 409)
	r, u, _ = ts.do(admin, "GET", "/api/admin/users/root", "", nil)
	expectStatus(t, "root retained", r, u, 200)
	if u["display_name"] == "Must not save" || u["is_admin"] != true || u["disabled"] != false {
		t.Fatal("last-admin refusal partially changed user")
	}
	r, u, _ = ts.do(admin, "DELETE", "/api/admin/users/root", ts.csrf(admin), nil)
	expectStatus(t, "last admin delete", r, u, 409)
}

func TestCreateUserWithDisabledProperty(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "password")
	admin := newClient(t)
	ts.login(admin, "root", "password")
	r, u, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]any{"username": "blocked", "password": "password", "disabled": true})
	expectStatus(t, "create disabled", r, u, 201)
	r, u, _ = ts.do(admin, "GET", "/api/admin/users/blocked", "", nil)
	expectStatus(t, "read disabled", r, u, 200)
	if u["disabled"] != true {
		t.Fatal("create ignored the disabled property")
	}
}

func TestDeleteUserRequiresRemovingOwnedSwarms(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "password")
	admin := newClient(t)
	ts.login(admin, "root", "password")
	ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]string{"username": "member", "password": "password"})
	r, p, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "p"})
	expectStatus(t, "project", r, p, 201)
	prefix := "/api/projects/" + p["id"].(string)
	r, u, _ := ts.do(admin, "POST", prefix+"/members", ts.csrf(admin), map[string]string{"username": "member"})
	expectStatus(t, "membership", r, u, 204)
	member := newClient(t)
	ts.login(member, "member", "password")
	r, sw, _ := ts.do(member, "POST", prefix+"/swarms", ts.csrf(member), map[string]string{"name": "sw"})
	expectStatus(t, "swarm", r, sw, 201)
	r, u, _ = ts.do(admin, "DELETE", "/api/admin/users/member", ts.csrf(admin), nil)
	expectStatus(t, "live owner", r, u, http.StatusConflict)
	r, u, _ = ts.do(admin, "DELETE", "/api/swarms/"+sw["id"].(string), ts.csrf(admin), nil)
	expectStatus(t, "remove swarm", r, u, 204)
	r, u, _ = ts.do(admin, "DELETE", "/api/admin/users/member", ts.csrf(admin), nil)
	expectStatus(t, "remove owner", r, u, 204)
}
