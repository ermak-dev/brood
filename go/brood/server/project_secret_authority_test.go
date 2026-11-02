package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/projectsecret"
	"brood.dev/go/brood/store"
)

func TestProjectSecretAuthorityNonAdminManagement(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	admin, alice, bob, swarmID := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	swarm, err := ts.srv.store.GetSwarm(ctx, swarmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.PutProjectSecretVars(ctx, swarm.ProjectID, map[string]string{"APP_TOKEN": "synthetic-private-authority"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.PutProjectSecretFiles(ctx, swarm.ProjectID, []projectsecret.File{{Name: "private.txt", Data: []byte("synthetic-private-file")}}); err != nil {
		t.Fatal(err)
	}
	before, err := ts.srv.store.ReadProjectSecretBundle(ctx, swarm.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]string{"username": "outsider", "password": "outsider-password-1"})
	expectStatus(t, "create outsider", r, b, 201)
	outsider := newClient(t)
	ts.login(outsider, "outsider", "outsider-password-1")
	base := "/api/projects/" + swarm.ProjectID + "/secrets"
	for _, user := range []struct {
		name   string
		client *http.Client
	}{{"swarm-owner", alice}, {"project-member", bob}, {"outsider", outsider}} {
		t.Run(user.name, func(t *testing.T) {
			for _, route := range []struct{ method, suffix, body string }{
				{"GET", "", ""}, {"GET", "/vars", ""}, {"GET", "/files", ""},
				{"PUT", "/vars", `{"vars":{"APP_TOKEN":"synthetic-replacement"}}`},
				{"PUT", "/files", `{"files":[{"name":"private.txt","data":"eA=="}]}`},
				{"DELETE", "/vars", `{"names":["APP_TOKEN"]}`},
				{"DELETE", "/files", `{"names":["private.txt"]}`},
			} {
				var body any
				if route.body != "" {
					if err := json.Unmarshal([]byte(route.body), &body); err != nil {
						t.Fatal(err)
					}
				}
				r, _, raw := ts.do(user.client, route.method, base+route.suffix, ts.csrf(user.client), body)
				if r.StatusCode != 403 || strings.Contains(raw, "synthetic-") || strings.Contains(raw, "APP_TOKEN") || strings.Contains(raw, "private.txt") {
					t.Fatalf("%s %s: status=%d body=%s", route.method, route.suffix, r.StatusCode, raw)
				}
			}
		})
	}
	after, err := ts.srv.store.ReadProjectSecretBundle(ctx, swarm.ProjectID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("denied management changed stored secrets: %v", err)
	}
}

// Чтение начинается после middleware и первой проверки администратора, но до транзакции.
type projectSecretAuthorityReader struct {
	io.Reader
	beforeRead func()
}

func (r *projectSecretAuthorityReader) Read(p []byte) (int, error) {
	if hook := r.beforeRead; hook != nil {
		r.beforeRead = nil
		hook()
	}
	return r.Reader.Read(p)
}

func TestProjectSecretAuthorityRecheckedAfterBodyRead(t *testing.T) {
	for _, change := range []string{"revoked", "disabled", "demoted"} {
		for _, mutation := range []struct{ method, kind, body string }{
			{"PUT", "vars", `{"vars":{"APP_TOKEN":"synthetic-replacement"}}`},
			{"PUT", "files", `{"files":[{"name":"private.txt","data":"eA=="}]}`},
			{"DELETE", "vars", `{"names":["APP_TOKEN"]}`},
			{"DELETE", "files", `{"names":["private.txt"]}`},
		} {
			t.Run(change+"/"+mutation.method+"/"+mutation.kind, func(t *testing.T) {
				ts := newTestServer(t)
				ts.bootstrapAdmin("admin", "admin-password-1")
				admin := newClient(t)
				ts.login(admin, "admin", "admin-password-1")
				r, b, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "authority"})
				expectStatus(t, "project", r, b, 201)
				id := b["id"].(string)
				ctx := context.Background()
				if err := ts.srv.store.CreateUser(ctx, store.User{ID: "spare-admin", Username: "spare-admin", PasswordHash: "test-only", IsAdmin: true}); err != nil {
					t.Fatal(err)
				}
				user, err := ts.srv.store.GetUserByUsername(ctx, "admin")
				if err != nil {
					t.Fatal(err)
				}
				if err := ts.srv.store.PutProjectSecretVars(ctx, id, map[string]string{"APP_TOKEN": "synthetic-original"}); err != nil {
					t.Fatal(err)
				}
				if err := ts.srv.store.PutProjectSecretFiles(ctx, id, []projectsecret.File{{Name: "private.txt", Data: []byte("synthetic-original-file")}}); err != nil {
					t.Fatal(err)
				}
				before, err := ts.srv.store.ReadProjectSecretBundle(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				read := false
				reader := &projectSecretAuthorityReader{Reader: strings.NewReader(mutation.body), beforeRead: func() {
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
				req := httptest.NewRequest(mutation.method, ts.http.URL+"/api/projects/"+id+"/secrets/"+mutation.kind, reader)
				req.Header.Set("Origin", ts.http.URL)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set(auth.HeaderCSRF, ts.csrf(admin))
				for _, cookie := range admin.Jar.Cookies(req.URL) {
					req.AddCookie(cookie)
				}
				w := httptest.NewRecorder()
				ts.srv.Handler().ServeHTTP(w, req)
				if !read || w.Code != 403 || !strings.Contains(w.Body.String(), "authorization changed") || strings.Contains(w.Body.String(), "synthetic-") {
					t.Fatalf("late %s: read=%v status=%d body=%s", change, read, w.Code, w.Body.String())
				}
				after, err := ts.srv.store.ReadProjectSecretBundle(ctx, id)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("late %s changed stored secrets: %v", change, err)
				}
			})
		}
	}
}

func TestProjectSecretAuthorityGuestBindings(t *testing.T) {
	ts, admin, actor, _ := masterSetup(t)
	ctx := context.Background()
	swarm, err := ts.srv.store.GetSwarm(ctx, actor.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.PutProjectSecretVars(ctx, swarm.ProjectID, map[string]string{"APP_TOKEN": "synthetic-own-project"}); err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "foreign-secrets"})
	expectStatus(t, "foreign project", r, b, 201)
	if err := ts.srv.store.PutProjectSecretVars(ctx, b["id"].(string), map[string]string{"FOREIGN_TOKEN": "synthetic-foreign-project"}); err != nil {
		t.Fatal(err)
	}
	for projectID, name := range map[string]string{swarm.ProjectID: "own.txt", b["id"].(string): "foreign.txt"} {
		if err := ts.srv.store.PutProjectSecretFiles(ctx, projectID, []projectsecret.File{{Name: name, Data: []byte("synthetic-" + name)}}); err != nil {
			t.Fatal(err)
		}
	}
	fetch := func(t *testing.T, uid string, generation int64, token string, want int) {
		t.Helper()
		req := httptest.NewRequest("GET", fmt.Sprintf("/api/proxmox/secrets/%s/%d", uid, generation), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		ts.srv.Handler().ServeHTTP(w, req)
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "synthetic-foreign-project") || strings.Contains(w.Body.String(), "FOREIGN_TOKEN") || strings.Contains(w.Body.String(), "foreign.txt") {
			t.Fatalf("guest status=%d want=%d body=%s", w.Code, want, w.Body.String())
		}
		if want != 200 {
			if strings.Contains(w.Body.String(), "synthetic-own-project") || strings.Contains(w.Body.String(), "APP_TOKEN") || strings.Contains(w.Body.String(), "own.txt") {
				t.Fatal("denied guest received project data")
			}
			return
		}
		var bundle projectsecret.Bundle
		if err := json.Unmarshal(w.Body.Bytes(), &bundle); err != nil || len(bundle.Vars) != 1 || bundle.Vars["APP_TOKEN"] != "synthetic-own-project" {
			t.Fatalf("guest did not receive exactly its own project variables: %v", err)
		}
		if len(bundle.Files) != 1 || bundle.Files[0].Name != "own.txt" || string(bundle.Files[0].Data) != "synthetic-own.txt" {
			t.Fatal("guest did not receive exactly its own project files")
		}
	}
	token := guestToken(ts.srv.managed.registry.relaySecret, actor.UID, actor.Incarnation)
	fetch(t, actor.UID, actor.Incarnation, token, 200)
	t.Run("wrong-uid", func(t *testing.T) { fetch(t, "unknown-actor", actor.Incarnation, token, 401) })
	t.Run("unknown-uid-signed-token", func(t *testing.T) {
		fetch(t, "unknown-actor", actor.Incarnation, guestToken(ts.srv.managed.registry.relaySecret, "unknown-actor", actor.Incarnation), 401)
	})
	t.Run("wrong-token", func(t *testing.T) { fetch(t, actor.UID, actor.Incarnation, strings.Repeat("x", 43), 401) })
	t.Run("wrong-generation-signed-token", func(t *testing.T) {
		fetch(t, actor.UID, actor.Incarnation+1, guestToken(ts.srv.managed.registry.relaySecret, actor.UID, actor.Incarnation+1), 401)
	})
	t.Run("changed-namespace", func(t *testing.T) {
		ts.fake.mu.Lock()
		metadata := ts.fake.atespaces[swarm.AtespaceName].Metadata
		oldUID := metadata.Uid
		metadata.Uid = "replacement-namespace"
		ts.fake.mu.Unlock()
		defer func() { ts.fake.mu.Lock(); metadata.Uid = oldUID; ts.fake.mu.Unlock() }()
		fetch(t, actor.UID, actor.Incarnation, token, 503)
	})
	// Наблюдаемая фаза не отменяет start/running intent: гостю нужны секреты при загрузке.
	actor.Phase = "stopped"
	actor, err = ts.srv.store.UpdateManagedActor(ctx, actor, actor.Revision)
	if err != nil {
		t.Fatal(err)
	}
	fetch(t, actor.UID, actor.Incarnation, token, 200)
	actor.Phase = "running"
	actor, err = ts.srv.store.UpdateManagedActor(ctx, actor, actor.Revision)
	if err != nil {
		t.Fatal(err)
	}
	oldGeneration := actor.Incarnation
	actorPath := "/api/swarms/" + actor.SwarmID + "/actors/" + actor.Name
	r, b, _ = ts.do(admin, "POST", actorPath, ts.csrf(admin), map[string]string{"action": "reset"})
	expectStatus(t, "reset intent", r, b, 200)
	fetch(t, actor.UID, oldGeneration, token, 401)
	for range 10 {
		ts.srv.managed.reconcileOnce(ctx)
	}
	actor, err = ts.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || actor.Incarnation != oldGeneration+1 || actor.Phase != "running" {
		t.Fatalf("reset did not produce a new running incarnation: generation=%d phase=%s stage=%s detail=%s err=%v", actor.Incarnation, actor.Phase, actor.Stage, actor.Error, err)
	}
	fetch(t, actor.UID, oldGeneration, token, 401)
	token = guestToken(ts.srv.managed.registry.relaySecret, actor.UID, actor.Incarnation)
	fetch(t, actor.UID, actor.Incarnation, token, 200)
	r, b, _ = ts.do(admin, "POST", actorPath, ts.csrf(admin), map[string]string{"action": "stop"})
	expectStatus(t, "stop intent", r, b, 200)
	fetch(t, actor.UID, actor.Incarnation, token, 401)
	for range 4 {
		ts.srv.managed.reconcileOnce(ctx)
	}
	actor, err = ts.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || actor.Phase != "stopped" || actor.Desired != "stopped" {
		t.Fatalf("stop not reconciled: phase=%s desired=%s err=%v", actor.Phase, actor.Desired, err)
	}
	fetch(t, actor.UID, actor.Incarnation, token, 401)
}
