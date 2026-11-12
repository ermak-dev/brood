package clientapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const swarmRolesBody = `{"swarmId":"s1","execution":"idle","graph":{"pinned":false,"name":"release","version":3,"revision":"rev"},"roles":[{"role":"build","inGraph":true,"state":"ready","actorUid":"u1","actorName":"master","actorIncarnation":2},{"role":"review","inGraph":true,"state":"unbound"}]}`

// FEATURE: Client transport for swarm role bindings (#235).
// SPEC: GET, PUT and DELETE /api/swarms/{id}/roles over the normal session
// and CSRF transport; path segments are escaped; the PUT body names exactly
// actorUid and, when known, actorIncarnation; only managed actors are
// offered for a role.
// IMPLEMENTATION: Typed role view, explicit wire body, managed rows of the
// swarm actor list.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestSwarmRoleTransport(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		if r.Method != "GET" && (r.Header.Get("X-CSRFToken") != "csrf" || r.Header.Get("Origin") == "") {
			t.Error("missing normal CSRF transport")
		}
		if r.Method == "PUT" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			want := map[string]any{"actorUid": "u1", "actorIncarnation": float64(2)}
			if strings.HasSuffix(r.URL.Path, "/deploy") {
				want = map[string]any{"actorUid": "u1"}
			}
			if !reflect.DeepEqual(body, want) {
				t.Errorf("PUT body = %v", body)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/actors") {
			fmt.Fprint(w, `{"results":[{"metadata":{"name":"native","uid":"n1"}},{"metadata":{"name":"master","uid":"u1"},"provider":"proxmox","generation":"2","phase":"running"}]}`)
			return
		}
		fmt.Fprint(w, swarmRolesBody)
	}))
	defer server.Close()
	c, _ := New(server.URL)
	c.Restore(StoredSession{Session: "session", CSRF: "csrf"})
	ctx := context.Background()
	want := SwarmRoles{SwarmID: "s1", Execution: "idle", Graph: &SwarmRoleGraph{Name: "release", Version: 3, Revision: "rev"}, Roles: []SwarmRole{
		{Role: "build", InGraph: true, State: "ready", ActorUID: "u1", ActorName: "master", ActorIncarnation: 2},
		{Role: "review", InGraph: true, State: "unbound"},
	}}
	for name, call := range map[string]func() (SwarmRoles, error){
		"list":                     func() (SwarmRoles, error) { return c.SwarmRoles(ctx, "s/1", "") },
		"list catalog":             func() (SwarmRoles, error) { return c.SwarmRoles(ctx, "s1", "rel ease") },
		"bind":                     func() (SwarmRoles, error) { return c.BindSwarmRole(ctx, "s1", "build", "u1", 2) },
		"bind without incarnation": func() (SwarmRoles, error) { return c.BindSwarmRole(ctx, "s1", "deploy", "u1", 0) },
		"unbind":                   func() (SwarmRoles, error) { return c.UnbindSwarmRole(ctx, "s1", "build") },
	} {
		got, err := call()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %+v, %v", name, got, err)
		}
	}
	actors, err := c.SwarmActors(ctx, "s1")
	if err != nil || !reflect.DeepEqual(actors, []SwarmActor{{UID: "u1", Name: "master", Incarnation: 2, Phase: "running"}}) {
		t.Fatalf("actors = %+v, %v", actors, err)
	}
	wantRequests := map[string]bool{
		"GET /api/swarms/s%2F1/roles?":                 true,
		"GET /api/swarms/s1/roles?definition=rel+ease": true,
		"PUT /api/swarms/s1/roles/build?":              true,
		"PUT /api/swarms/s1/roles/deploy?":             true,
		"DELETE /api/swarms/s1/roles/build?":           true,
		"GET /api/swarms/s1/actors?":                   true,
	}
	for _, request := range requests {
		if !wantRequests[request] {
			t.Errorf("unexpected request %s", request)
		}
	}
	if len(requests) != len(wantRequests) {
		t.Fatalf("requests: %v", requests)
	}
}

// TestSwarmRoleRefusalReasons — фиксированная причина отказа сервера доходит
// до вызывающего, любой другой текст тела отбрасывается.
func TestSwarmRoleRefusalReasons(t *testing.T) {
	detail := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"detail": detail})
	}))
	defer server.Close()
	c, _ := New(server.URL)
	c.Restore(StoredSession{Session: "session", CSRF: "csrf"})
	ctx := context.Background()
	for _, tc := range []struct {
		detail string
		shown  bool
	}{
		{"actor is not live: it must be provisioning or running and meant to run", true},
		{"actor is waiting to stop for a reset or update; bind it once it runs again", true},
		{"swarm execution is running or waiting; stop the swarm before changing role bindings", true},
		{"swarm role is not bound", true},
		{"synthetic-secret-sentinel", false},
		{"actor is not live: it must be provisioning or running and meant to run\x1b[2J", false},
	} {
		detail = tc.detail
		_, err := c.BindSwarmRole(ctx, "s1", "build", "u1", 0)
		if err == nil || strings.Contains(err.Error(), tc.detail) != tc.shown || strings.Contains(err.Error(), "sentinel") || strings.Contains(err.Error(), "\x1b") {
			t.Fatalf("detail %q: %v", tc.detail, err)
		}
		if _, err := c.UnbindSwarmRole(ctx, "s1", "build"); err == nil || strings.Contains(err.Error(), tc.detail) != tc.shown {
			t.Fatalf("unbind detail %q: %v", tc.detail, err)
		}
	}
}

// TestSwarmRoleInputChecked — пустые обязательные значения отвергаются до
// запроса.
func TestSwarmRoleInputChecked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid input made a request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	c, _ := New(server.URL)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"roles without swarm":  func() error { _, err := c.SwarmRoles(ctx, "", ""); return err },
		"bind without role":    func() error { _, err := c.BindSwarmRole(ctx, "s1", "", "u1", 0); return err },
		"bind without actor":   func() error { _, err := c.BindSwarmRole(ctx, "s1", "build", "", 0); return err },
		"negative incarnation": func() error { _, err := c.BindSwarmRole(ctx, "s1", "build", "u1", -1); return err },
		"unbind without role":  func() error { _, err := c.UnbindSwarmRole(ctx, "s1", ""); return err },
		"actors without swarm": func() error { _, err := c.SwarmActors(ctx, ""); return err },
	} {
		if call() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
