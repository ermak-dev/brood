package server

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func gitCredentialBody(name, repository, token string) map[string]string {
	return map[string]string{"name": name, "repository": repository, "username": "brood-bot", "token": token}
}

// FEATURE: Write-only API for server-only project Git credentials (#173).
// SPEC: Only a fresh administrator with CSRF manages credentials; requests are
// exactly {"name","repository","username","token"} or {"names":[...]};
// responses and refusals list names, bound repositories and the active flag,
// never the username or token. The repository is canonicalized before binding.
// IMPLEMENTATION: Exact-field token scan, shared validators, transactional
// session and role recheck before the write.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestProjectGitCredentialAPI(t *testing.T) {
	ts, admin, alice, carol, id := gitRepositorySetup(t)
	path := "/api/projects/" + id + "/secrets/git"
	noLeak := func(name string, raw string) {
		t.Helper()
		if strings.Contains(raw, gitTokenSentinel) || strings.Contains(raw, "brood-bot") {
			t.Fatalf("%s disclosed the credential: %s", name, raw)
		}
	}
	r, b, raw := ts.do(admin, "GET", path, "", nil)
	expectStatus(t, "empty list", r, b, http.StatusOK)
	if raw != `{"items":[]}`+"\n" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("empty list: %q %v", raw, r.Header)
	}
	r, b, raw = ts.do(admin, "PUT", path, ts.csrf(admin), gitCredentialBody("gitlab", "https://Git.Example.com/group/repo.git/", gitTokenSentinel))
	expectStatus(t, "put", r, b, http.StatusOK)
	noLeak("put", raw)
	want := map[string]any{"items": []any{map[string]any{"name": "gitlab", "repository": gitTestOrigin, "active": false}}}
	if !reflect.DeepEqual(b, want) || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("put response: %s", raw)
	}
	ctx := context.Background()
	if err := ts.srv.store.SetProjectRepository(ctx, id, gitTestOrigin); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetProjectGitCredentialRef(ctx, id, "gitlab"); err != nil {
		t.Fatal(err)
	}
	access, err := ts.srv.store.ProjectGitAccess(ctx, id)
	if err != nil || access.Credential.Token != gitTokenSentinel || access.Credential.Repository != gitTestOrigin {
		t.Fatalf("stored credential: %v %v", access.HasCredential(), err)
	}
	r, b, raw = ts.do(admin, "GET", path, "", nil)
	expectStatus(t, "list", r, b, http.StatusOK)
	noLeak("list", raw)
	want = map[string]any{"items": []any{map[string]any{"name": "gitlab", "repository": gitTestOrigin, "active": true}}}
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("list: %s", raw)
	}

	// Authorization: members, outsiders, anonymous clients and CSRF-less requests are refused.
	for _, tc := range []struct {
		name   string
		method string
		client *http.Client
		csrf   string
		want   int
	}{
		{"member list", "GET", alice, "", http.StatusForbidden},
		{"outsider list", "GET", carol, "", http.StatusForbidden},
		{"anonymous list", "GET", newClient(t), "", http.StatusUnauthorized},
		{"member put", "PUT", alice, ts.csrf(alice), http.StatusForbidden},
		{"member delete", "DELETE", alice, ts.csrf(alice), http.StatusForbidden},
		{"admin put without CSRF", "PUT", admin, "", http.StatusForbidden},
		{"admin delete without CSRF", "DELETE", admin, "", http.StatusForbidden},
	} {
		var body any
		switch tc.method {
		case "PUT":
			body = gitCredentialBody("other", gitTestOrigin, "synthetic-other-token")
		case "DELETE":
			body = map[string]any{"names": []string{"gitlab"}}
		}
		r, _, raw := ts.do(tc.client, tc.method, path, tc.csrf, body)
		if r.StatusCode != tc.want || strings.Contains(raw, gitTestOrigin) {
			t.Fatalf("%s: %d %s", tc.name, r.StatusCode, raw)
		}
	}
	r, _, _ = ts.do(admin, "GET", "/api/projects/missing/secrets/git", "", nil)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("missing project: %d", r.StatusCode)
	}

	// Strict structure: 400 before any write, without echo.
	for _, body := range []string{
		`{"name":"x","repository":"` + gitTestOrigin + `","username":"brood-bot"}`,
		`{"name":"x","repository":"` + gitTestOrigin + `","username":"brood-bot","token":"` + gitTokenSentinel + `","extra":""}`,
		`{"name":"x","repository":"` + gitTestOrigin + `","username":"brood-bot","Token":"` + gitTokenSentinel + `"}`,
		`{"name":"x","repository":"` + gitTestOrigin + `","username":"brood-bot","token":"` + gitTokenSentinel + `","token":"` + gitTokenSentinel + `"}`,
		`{"name":"x","repository":"` + gitTestOrigin + `","username":"brood-bot","token":null}`,
		`{"name":"x","repository":"` + gitTestOrigin + `","username":"brood-bot","token":1}`,
		`{"name":"x","repository":"` + gitTestOrigin + `","username":"brood-bot","token":"` + gitTokenSentinel + `"} {}`,
		`["` + gitTokenSentinel + `"]`,
		``,
	} {
		r, _, raw := ts.doRaw(admin, "PUT", path, ts.csrf(admin), body)
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s: %d %s", body, r.StatusCode, raw)
		}
		noLeak("structure refusal", raw)
	}
	// Invalid values: 422 naming the rule, never the value.
	for _, body := range []map[string]string{
		gitCredentialBody("Bad", gitTestOrigin, "synthetic-other-token"),
		gitCredentialBody("other", "https://brood-bot:"+gitTokenSentinel+"@git.example.com/group/repo.git", "synthetic-other-token"),
		gitCredentialBody("other", "http://git.example.com/group/repo.git", "synthetic-other-token"),
		gitCredentialBody("other", gitTestOrigin, gitTokenSentinel+" "),
		{"name": "other", "repository": gitTestOrigin, "username": "brood-bot:" + gitTokenSentinel, "token": "synthetic-other-token"},
	} {
		r, _, raw := ts.do(admin, "PUT", path, ts.csrf(admin), body)
		if r.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("invalid value: %d %s", r.StatusCode, raw)
		}
		noLeak("value refusal", raw)
	}
	for _, body := range []string{`{"names":[]}`, `{"names":["Bad"]}`, `{"names":"gitlab"}`, `{"names":["gitlab"],"x":1}`, `{"Names":["gitlab"]}`, `{"names":["gitlab"],"names":["gitlab"]}`} {
		r, _, raw := ts.doRaw(admin, "DELETE", path, ts.csrf(admin), body)
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("delete body %s: %d %s", body, r.StatusCode, raw)
		}
	}
	if infos, err := ts.srv.store.ProjectGitCredentials(ctx, id); err != nil || len(infos) != 1 {
		t.Fatalf("refused requests changed credentials: %+v %v", infos, err)
	}

	// Rotation keeps the name; removal revokes the gateway credential.
	r, b, raw = ts.do(admin, "PUT", path, ts.csrf(admin), gitCredentialBody("gitlab", gitTestOrigin, "synthetic-rotated-token"))
	expectStatus(t, "rotate", r, b, http.StatusOK)
	noLeak("rotate", raw)
	if access, err := ts.srv.store.ProjectGitAccess(ctx, id); err != nil || access.Credential.Token != "synthetic-rotated-token" {
		t.Fatalf("rotation: %v", err)
	}
	r, b, raw = ts.do(admin, "DELETE", path, ts.csrf(admin), map[string]any{"names": []string{"gitlab", "absent"}})
	expectStatus(t, "delete", r, b, http.StatusOK)
	if raw != `{"items":[]}`+"\n" {
		t.Fatalf("delete response: %s", raw)
	}
	if access, err := ts.srv.store.ProjectGitAccess(ctx, id); err != nil || access.HasCredential() {
		t.Fatalf("removed credential still resolves: %v", err)
	}
}
