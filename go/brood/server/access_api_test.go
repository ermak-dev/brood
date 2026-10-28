package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/app"
)

// FEATURE: Expose local swarm roles and safe member choices over HTTP (#49).
// SPEC: Explicit readers see roles/resources, only owners grant/revoke/transfer;
// membership alone reveals no swarm, and all mutations retain CSRF protection.
func TestSwarmMemberHTTPRejectsIncompleteRequests(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, _, swarm := actorsSetup(t, ts.testServer)
	for _, body := range []map[string]string{{}, {"role": "viewer"}, {"username": "bob"}, {"username": " ", "role": "viewer"}} {
		r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/members", ts.csrf(alice), body)
		expectStatus(t, "incomplete grant", r, b, 400)
	}
}

func TestSwarmMemberHTTPContract(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	admin, alice, bob, swarm := actorsSetup(t, ts.testServer)
	base := "/api/swarms/" + swarm
	r, b, _ := ts.do(alice, "GET", base, "", nil)
	expectStatus(t, "owner swarm", r, b, 200)
	if b["role"] != "owner" {
		t.Fatalf("effective owner role missing: %v", b)
	}
	r, b, raw := ts.do(alice, "GET", base+"/members", "", nil)
	expectStatus(t, "owner grants", r, b, 200)
	var members []map[string]any
	if err := json.Unmarshal([]byte(raw), &members); err != nil || len(members) != 1 || members[0]["username"] != "alice" || members[0]["role"] != "owner" {
		t.Fatalf("wrong grant response: %s, %v", raw, err)
	}
	for _, secret := range []string{"password", "hash", "session"} {
		if strings.Contains(raw, secret) {
			t.Fatal("grant response disclosed authentication state")
		}
	}
	r, b, _ = ts.do(bob, "GET", base+"/members", "", nil)
	expectStatus(t, "membership alone hidden", r, b, 404)
	r, b, _ = ts.do(alice, "POST", base+"/members", ts.csrf(alice), map[string]string{"username": "bob", "role": "viewer"})
	expectStatus(t, "grant viewer", r, b, 204)
	r, b, _ = ts.do(bob, "GET", base, "", nil)
	expectStatus(t, "viewer swarm", r, b, 200)
	if b["role"] != "viewer" {
		t.Fatalf("viewer role missing: %v", b)
	}
	r, b, _ = ts.do(bob, "GET", base+"/members", "", nil)
	expectStatus(t, "viewer grants", r, b, 200)
	r, b, _ = ts.do(bob, "POST", base+"/members", ts.csrf(bob), map[string]string{"username": "bob", "role": "owner"})
	expectStatus(t, "viewer escalation denied", r, b, 403)
	r, b, _ = ts.do(alice, "POST", base+"/members", "", map[string]string{"username": "bob", "role": "editor"})
	expectStatus(t, "csrf", r, b, 403)
	r, b, _ = ts.do(alice, "POST", base+"/members", ts.csrf(alice), map[string]string{"username": "bob", "role": "administrator"})
	expectStatus(t, "invalid role", r, b, 400)
	r, b, _ = ts.do(alice, "POST", base+"/members", ts.csrf(alice), map[string]any{"username": "bob", "role": "editor", "is_admin": true})
	expectStatus(t, "unknown grant field", r, b, 400)
	r, b, _ = ts.do(alice, "DELETE", base+"/members/alice", ts.csrf(alice), nil)
	expectStatus(t, "last owner", r, b, 409)
	r, b, _ = ts.do(alice, "DELETE", base+"/members/bob", ts.csrf(alice), nil)
	expectStatus(t, "revoke viewer", r, b, 204)
	r, b, _ = ts.do(bob, "GET", base, "", nil)
	expectStatus(t, "revoked hidden", r, b, 404)
	r, b, _ = ts.do(admin, "GET", base, "", nil)
	expectStatus(t, "admin effective owner", r, b, 200)
	if b["role"] != "owner" {
		t.Fatalf("admin role missing: %v", b)
	}
}

func TestSwarmReaderCanReadButNotMutateNativeResources(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, bob, swarm := actorsSetup(t, ts.testServer)
	user, err := ts.srv.store.GetUserByUsername(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetSwarmMember(context.Background(), swarm, user.ID, app.RoleViewer); err != nil {
		t.Fatal(err)
	}
	base := "/api/swarms/" + swarm
	for _, resource := range []string{"actors", "templates"} {
		r, b, _ := ts.do(bob, "GET", base+"/"+resource, "", nil)
		expectStatus(t, "viewer read "+resource, r, b, 200)
		r, b, _ = ts.do(bob, "POST", base+"/"+resource, ts.csrf(bob), map[string]any{})
		expectStatus(t, "viewer mutation "+resource, r, b, 404)
	}
	r, b, _ := ts.do(bob, "GET", base+"/policy", "", nil)
	expectStatus(t, "native policy stays admin only", r, b, 403)
	r, b, _ = ts.do(alice, "POST", base+"/transfer", ts.csrf(alice), map[string]string{"username": "bob"})
	expectStatus(t, "owner transfer", r, b, 204)
	r, b, _ = ts.do(alice, "GET", base, "", nil)
	expectStatus(t, "previous primary grant removed", r, b, 404)
	r, b, _ = ts.do(bob, "GET", base, "", nil)
	expectStatus(t, "new owner", r, b, 200)
	if b["owner_username"] != "bob" || b["role"] != "owner" {
		t.Fatalf("transfer projection wrong: %v", b)
	}
}

func TestProjectMemberChoicesVisibleWithoutForeignSwarms(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	admin, alice, bob, swarm := actorsSetup(t, ts.testServer)
	r, b, _ := ts.do(alice, "GET", "/api/swarms/"+swarm, "", nil)
	expectStatus(t, "own swarm", r, b, 200)
	project := b["project_id"].(string)
	for _, client := range []*http.Client{alice, bob} {
		r, b, raw := ts.do(client, "GET", "/api/projects/"+project+"/members", "", nil)
		expectStatus(t, "project member choices", r, b, 200)
		var members []map[string]any
		if err := json.Unmarshal([]byte(raw), &members); err != nil || len(members) != 2 {
			t.Fatalf("wrong member choices: %s, %v", raw, err)
		}
		if strings.Contains(raw, "password") || strings.Contains(raw, "swarm-") {
			t.Fatalf("member choices disclosed unrelated data: %s", raw)
		}
	}
	r, b, _ = ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]string{"username": "outsider", "password": "outsider-password-1"})
	expectStatus(t, "create outsider", r, b, 201)
	outsider := newClient(t)
	ts.login(outsider, "outsider", "outsider-password-1")
	r, b, _ = ts.do(outsider, "GET", "/api/projects/"+project+"/members", "", nil)
	expectStatus(t, "outsider choices hidden", r, b, 404)
	r, b, _ = ts.do(bob, "GET", "/api/swarms/"+swarm, "", nil)
	expectStatus(t, "choices did not reveal foreign swarm", r, b, 404)
}
