package server

// FEATURE: Explicit administrator activation and lifecycle of managed pools.
// SPEC: Installed immutable configuration only; no native worker synthesis,
// no GET writes, cookie/CSRF protection, persistent identity, explicit drain.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxmoxPoolActivationAndProtection(t *testing.T) {
	cfg, err := loadProxmox(runtimeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBPath = t.TempDir() + "/brood.db"
	srv, err := New(nil, cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{t: t, srv: srv, http: httptest.NewServer(srv.Handler())}
	defer func() { ts.http.Close(); srv.Close() }()
	path := "/api/proxmox-pools/brood-proxmox/activate"
	anon := newClient(t)
	r, b, _ := ts.do(anon, "POST", path, "", nil)
	expectStatus(t, "anonymous", r, b, 401)
	ts.bootstrapAdmin("admin", "admin-password-1")
	admin := newClient(t)
	ts.login(admin, "admin", "admin-password-1")
	r, b, _ = ts.do(admin, "POST", path, "", nil)
	expectStatus(t, "CSRF", r, b, 403)
	r, b, _ = ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]string{"username": "ordinary", "password": "ordinary-password-1"})
	expectStatus(t, "new user", r, b, 201)
	ordinary := newClient(t)
	ts.login(ordinary, "ordinary", "ordinary-password-1")
	r, b, _ = ts.do(ordinary, "POST", path, ts.csrf(ordinary), nil)
	expectStatus(t, "ordinary", r, b, 403)
	r, b, _ = ts.do(admin, "GET", "/api/proxmox-pools", "", nil)
	expectStatus(t, "read", r, b, 200)
	if b["items"].([]any)[0].(map[string]any)["managedPool"] != nil {
		t.Fatal("GET activated pool")
	}
	r, b, _ = ts.do(admin, "POST", path, ts.csrf(admin), map[string]any{"template": "arbitrary"})
	expectStatus(t, "arbitrary configuration", r, b, 400)
	r, b, _ = ts.do(admin, "POST", path, ts.csrf(admin), nil)
	expectStatus(t, "activate", r, b, http.StatusCreated)
	pool := b["managedPool"].(map[string]any)
	uid := pool["uid"]
	if uid == "" || pool["state"] != "enabled" || pool["maxActors"] != "2" || pool["usedActors"] != "0" {
		t.Fatalf("wrong activation DTO: %#v", pool)
	}
	r, b, _ = ts.do(admin, "POST", path, ts.csrf(admin), nil)
	expectStatus(t, "idempotent activation", r, b, 200)
	if b["managedPool"].(map[string]any)["uid"] != uid {
		t.Fatal("activation changed pool UID")
	}
	for _, state := range []string{"drain", "enable"} {
		r, b, _ = ts.do(admin, "POST", "/api/proxmox-pools/brood-proxmox/"+state, ts.csrf(admin), nil)
		expectStatus(t, state, r, b, 200)
		want := "enabled"
		if state == "drain" {
			want = "draining"
		}
		if b["managedPool"].(map[string]any)["state"] != want {
			t.Fatalf("wrong state: %#v", b)
		}
	}
	r, b, raw := ts.do(admin, "GET", "/api/proxmox-pools", "", nil)
	expectStatus(t, "inventory", r, b, 200)
	if strings.Contains(raw, proxmoxSentinel) || strings.Contains(raw, "credentialRef") || strings.Contains(raw, "caPEM") {
		t.Fatal("private configuration leaked")
	}
	if srv.pools != nil {
		t.Fatal("managed activation created native worker pool")
	}
	ts.http.Close()
	srv.Close()
	srv, err = New(nil, cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts.srv = srv
	ts.http = httptest.NewServer(srv.Handler())
	// Authenticate again: random session-secret defaults are deliberately not a restart guarantee.
	admin = newClient(t)
	ts.login(admin, "admin", "admin-password-1")
	r, b, _ = ts.do(admin, "GET", "/api/proxmox-pools", "", nil)
	expectStatus(t, "restart inventory", r, b, 200)
	if b["items"].([]any)[0].(map[string]any)["managedPool"].(map[string]any)["uid"] != uid {
		t.Fatal("restart lost managed identity")
	}
	r, b, _ = ts.do(admin, "DELETE", "/api/proxmox-pools/brood-proxmox", ts.csrf(admin), nil)
	expectStatus(t, "delete empty pool", r, b, 204)
}
