package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestAdministratorRoleManagement(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("root", "password", true)
	target := ts.addUser("alice", "password", false)
	if ts.login("alice", "password") != http.StatusNoContent {
		t.Fatal("target login failed")
	}
	oldSession, oldCSRF := ts.session, ts.csrf
	if ts.login("root", "password") != http.StatusNoContent {
		t.Fatal("admin login failed")
	}
	for _, body := range []string{`{}`, `{"is_admin":null}`} {
		response := ts.do("POST", "/api/admin/users/alice/role", body, nil)
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("missing role: got %d", response.StatusCode)
		}
	}
	response := ts.do("POST", "/api/admin/users/alice/role", `{"is_admin":true}`, nil)
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("promote: got %d", response.StatusCode)
	}
	user, err := ts.svc.store.GetUser(context.Background(), target.ID)
	if err != nil || !user.IsAdmin {
		t.Fatalf("promotion not persisted: %v %v", user, err)
	}
	adminSession, adminCSRF := ts.session, ts.csrf
	ts.session, ts.csrf = oldSession, oldCSRF
	response = ts.do("GET", "/api/auth/session", "", nil)
	var session struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if session.Authenticated {
		t.Fatal("role elevation must revoke old sessions")
	}
	ts.session, ts.csrf = adminSession, adminCSRF
	response = ts.do("POST", "/api/admin/users/alice/role", `{"is_admin":false}`, nil)
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("demote: got %d", response.StatusCode)
	}
	response = ts.do("POST", "/api/admin/users/root/role", `{"is_admin":false}`, nil)
	response.Body.Close()
	if response.StatusCode != 409 {
		t.Fatalf("last admin protection: got %d", response.StatusCode)
	}
}

func TestOrdinaryUserCannotGrantAdministratorRole(t *testing.T) {
	ts := newTestServer(t)
	ts.addUser("alice", "password", false)
	ts.login("alice", "password")
	response := ts.do("POST", "/api/admin/users/alice/role", `{"is_admin":true}`, nil)
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatalf("ordinary role change: got %d", response.StatusCode)
	}
}
