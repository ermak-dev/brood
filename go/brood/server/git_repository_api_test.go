package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

const gitTestOrigin = "https://git.example.com/group/repo.git"

func gitTestRemotes() gitRemotes {
	return gitRemotes{
		gitTestOrigin:                   {username: "brood-bot", token: gitTokenSentinel},
		"https://github.com/org/public": {},
	}
}

// gitRepositorySetup: admin root, project members alice and bob, outsider carol.
func gitRepositorySetup(t *testing.T) (*testServer, *http.Client, *http.Client, *http.Client, string) {
	t.Helper()
	ts := newTestServer(t)
	ts.srv.git.remotes = gitTestRemotes()
	ts.bootstrapAdmin("root", "root-password-1")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	for _, name := range []string{"alice", "carol"} {
		r, b, _ := ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]any{"username": name, "password": name + "-password-1"})
		expectStatus(t, "user "+name, r, b, http.StatusCreated)
	}
	r, b, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "repo-project"})
	expectStatus(t, "project", r, b, http.StatusCreated)
	id := b["id"].(string)
	r, b, _ = ts.do(admin, "POST", "/api/projects/"+id+"/members", ts.csrf(admin), map[string]string{"username": "alice"})
	expectStatus(t, "member", r, b, http.StatusNoContent)
	alice, carol := newClient(t), newClient(t)
	ts.login(alice, "alice", "alice-password-1")
	ts.login(carol, "carol", "carol-password-1")
	return ts, admin, alice, carol, id
}

// FEATURE: Installation remotes and project repository API (#146).
// SPEC: GET /api/git-remotes is admin-only and never returns credentials;
// project members read {"origin"}; only an admin with CSRF selects a
// configured remote or "" through strict JSON.
// IMPLEMENTATION: Exact-field token scan, canonical origin, transactional recheck.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGitRemotesAPI(t *testing.T) {
	ts, admin, alice, _, _ := gitRepositorySetup(t)
	r, b, raw := ts.do(admin, "GET", "/api/git-remotes", "", nil)
	expectStatus(t, "remotes", r, b, http.StatusOK)
	want := map[string]any{"items": []any{
		map[string]any{"origin": "https://git.example.com/group/repo.git", "credentialConfigured": true},
		map[string]any{"origin": "https://github.com/org/public", "credentialConfigured": false},
	}}
	if !reflect.DeepEqual(b, want) || strings.Contains(raw, gitTokenSentinel) || strings.Contains(raw, "brood-bot") {
		t.Fatalf("remotes response: %s", raw)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("remotes response is cacheable")
	}
	r, _, raw = ts.do(alice, "GET", "/api/git-remotes", "", nil)
	if r.StatusCode != http.StatusForbidden || strings.Contains(raw, "git.example.com") {
		t.Fatalf("non-admin remotes: %d %s", r.StatusCode, raw)
	}
	r, _, _ = ts.do(newClient(t), "GET", "/api/git-remotes", "", nil)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous remotes: %d", r.StatusCode)
	}
	ts.srv.git.remotes = gitRemotes{}
	r, b, raw = ts.do(admin, "GET", "/api/git-remotes", "", nil)
	if r.StatusCode != http.StatusOK || raw != `{"items":[]}`+"\n" && raw != `{"items":[]}` {
		t.Fatalf("empty remotes: %d %s", r.StatusCode, raw)
	}
}

func TestProjectRepositoryAPI(t *testing.T) {
	ts, admin, alice, carol, id := gitRepositorySetup(t)
	path := "/api/projects/" + id + "/repository"
	for _, client := range []*http.Client{admin, alice} {
		r, b, _ := ts.do(client, "GET", path, "", nil)
		expectStatus(t, "read empty", r, b, http.StatusOK)
		if !reflect.DeepEqual(b, map[string]any{"origin": ""}) {
			t.Fatalf("empty repository: %v", b)
		}
	}
	r, _, _ := ts.do(carol, "GET", path, "", nil)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider read: %d", r.StatusCode)
	}
	r, _, _ = ts.do(admin, "GET", "/api/projects/missing/repository", "", nil)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("missing project: %d", r.StatusCode)
	}
	r, _, _ = ts.do(newClient(t), "GET", path, "", nil)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous read: %d", r.StatusCode)
	}
	r, _, _ = ts.do(admin, "PUT", path, "", map[string]string{"origin": gitTestOrigin})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("CSRF bypass: %d", r.StatusCode)
	}
	r, _, _ = ts.do(alice, "PUT", path, ts.csrf(alice), map[string]string{"origin": gitTestOrigin})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("member write: %d", r.StatusCode)
	}
	for _, origin := range []string{"https://git.example.com/group/other.git", "https://brood-bot:" + gitTokenSentinel + "@git.example.com/group/repo.git", "http://git.example.com/group/repo.git"} {
		r, _, raw := ts.do(admin, "PUT", path, ts.csrf(admin), map[string]string{"origin": origin})
		if r.StatusCode != http.StatusUnprocessableEntity || strings.Contains(raw, gitTokenSentinel) || strings.Contains(raw, origin) {
			t.Fatalf("invalid repository: %d %s", r.StatusCode, raw)
		}
	}
	r, b, raw := ts.do(admin, "PUT", path, ts.csrf(admin), map[string]string{"origin": "https://Git.Example.com/group/repo.git/"})
	expectStatus(t, "select", r, b, http.StatusOK)
	if !reflect.DeepEqual(b, map[string]any{"origin": gitTestOrigin}) || strings.Contains(raw, gitTokenSentinel) {
		t.Fatalf("selected repository: %s", raw)
	}
	r, b, _ = ts.do(alice, "GET", path, "", nil)
	if r.StatusCode != http.StatusOK || b["origin"] != gitTestOrigin {
		t.Fatalf("member read: %d %v", r.StatusCode, b)
	}
	r, _, _ = ts.do(admin, "PUT", "/api/projects/missing/repository", ts.csrf(admin), map[string]string{"origin": gitTestOrigin})
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("missing project write: %d", r.StatusCode)
	}
	r, b, _ = ts.do(admin, "PUT", path, ts.csrf(admin), map[string]string{"origin": ""})
	expectStatus(t, "clear", r, b, http.StatusOK)
	if got, err := ts.srv.store.ProjectRepository(context.Background(), id); err != nil || got != "" || b["origin"] != "" {
		t.Fatalf("cleared repository: %q %v %v", got, err, b)
	}
}

func TestProjectRepositoryStrictJSON(t *testing.T) {
	ts, admin, _, _, id := gitRepositorySetup(t)
	if err := ts.srv.store.SetProjectRepository(context.Background(), id, gitTestOrigin); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		``, `null`, `[]`, `"x"`, `{}`,
		`{"Origin":""}`, `{"ORIGIN":""}`,
		`{"origin":"","origin":""}`,
		`{"origin":"","Origin":"https://github.com/org/public"}`,
		`{"origin":"","extra":true}`,
		`{"origin":null}`, `{"origin":1}`, `{"origin":["x"]}`, `{"origin":{}}`,
		`{"origin":""} {}`, `{"origin":""}x`, `{"origin":"`,
		`{"origin":"` + strings.Repeat("a", 8192) + `"}`,
	} {
		req := httptest.NewRequest("PUT", ts.http.URL+"/api/projects/"+id+"/repository", strings.NewReader(body))
		req.Header.Set("Origin", ts.http.URL)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(auth.HeaderCSRF, ts.csrf(admin))
		for _, cookie := range admin.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		ts.srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status=%d %s", body, w.Code, w.Body.String())
		}
	}
	if got, err := ts.srv.store.ProjectRepository(context.Background(), id); err != nil || got != gitTestOrigin {
		t.Fatalf("refused body changed repository: %q %v", got, err)
	}
}

func TestProjectRepositoryAuthorityRecheckedAfterBodyRead(t *testing.T) {
	for _, change := range []string{"revoked", "disabled", "demoted"} {
		t.Run(change, func(t *testing.T) {
			ts, admin, _, _, id := gitRepositorySetup(t)
			ctx := context.Background()
			if err := ts.srv.store.CreateUser(ctx, store.User{ID: "spare-admin", Username: "spare-admin", PasswordHash: "test-only", IsAdmin: true}); err != nil {
				t.Fatal(err)
			}
			user, err := ts.srv.store.GetUserByUsername(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			read := false
			reader := &projectSecretAuthorityReader{Reader: bytes.NewReader([]byte(`{"origin":"` + gitTestOrigin + `"}`)), beforeRead: func() {
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
			req := httptest.NewRequest("PUT", ts.http.URL+"/api/projects/"+id+"/repository", reader)
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
			if got, err := ts.srv.store.ProjectRepository(ctx, id); err != nil || got != "" {
				t.Fatalf("late %s changed repository: %q %v", change, got, err)
			}
		})
	}
}
