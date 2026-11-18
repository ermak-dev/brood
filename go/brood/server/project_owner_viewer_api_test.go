package server

import (
	"context"
	"net/http"
	"testing"
)

// FEATURE: Ownership metadata связывает viewer с UID, не с именем пользователя.
// SPEC: Общая auth-панель хранит username; owner UI получает стабильную личность
// из той же authenticated выдачи, без доверия к имени или owner UID из формы.
func TestProjectOwnershipViewBindsCurrentViewerIdentity(t *testing.T) {
	ts, admin, alice, _, project := gitRepositorySetup(t)
	path := "/api/projects/" + project + "/owner"
	r, b, _ := ts.do(admin, "PUT", path, ts.csrf(admin), map[string]any{"revision": 0, "username": "alice"})
	expectStatus(t, "assign owner", r, b, http.StatusOK)
	owner, err := ts.srv.store.GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, viewer := range []struct {
		name   string
		client *http.Client
	}{{"root", admin}, {"alice", alice}} {
		user, err := ts.srv.store.GetUserByUsername(context.Background(), viewer.name)
		if err != nil {
			t.Fatal(err)
		}
		r, b, _ := ts.do(viewer.client, "GET", path, "", nil)
		expectStatus(t, "owner metadata", r, b, http.StatusOK)
		if b["ownerUid"] != owner.ID || b["viewerUid"] != user.ID {
			t.Fatalf("owner metadata confused current viewer and owner: %+v", b)
		}
		if r.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("viewer-bound ownership metadata may be cached")
		}
	}
}
