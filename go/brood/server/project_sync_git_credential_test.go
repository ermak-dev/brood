package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// FEATURE: Broodfile reference to a project Git credential through sync (#173).
// SPEC: PUT /api/projects/sync accepts git_credential as a credential name or
// "" and stores it idempotently; the response reports the effective reference,
// whether it is bound to the declared repository and the resulting origin
// mode. A repository change with the same reference unbinds the credential, so
// the old token is never sent to the new server. Values never appear.
// IMPLEMENTATION: Strict declaration field, transactional reference write,
// mode from the settled access snapshot.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestProjectSyncGitCredentialReference(t *testing.T) {
	ts, admin, alice, _, _ := gitRepositorySetup(t)
	ts.srv.git.remotes = gitRemotes{}
	ctx := context.Background()
	declaration := map[string]any{"name": "declared", "repository": gitTestOrigin, "git_credential": "gitlab"}
	r, b, raw := ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), declaration)
	expectStatus(t, "create with reference", r, b, http.StatusCreated)
	id := b["id"].(string)
	if b["git_credential"] != "gitlab" || b["git_credential_bound"] != false || b["origin_mode"] != "" {
		t.Fatalf("unbound reference: %s", raw)
	}
	r, b, _ = ts.do(admin, "PUT", "/api/projects/"+id+"/secrets/git", ts.csrf(admin), gitCredentialBody("gitlab", gitTestOrigin, gitTokenSentinel))
	expectStatus(t, "credential", r, b, http.StatusOK)
	r, b, raw = ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), declaration)
	expectStatus(t, "repeat", r, b, http.StatusOK)
	if b["changed"] != false || b["git_credential"] != "gitlab" || b["git_credential_bound"] != true || b["origin_mode"] != "gateway" || strings.Contains(raw, gitTokenSentinel) || strings.Contains(raw, "brood-bot") {
		t.Fatalf("bound reference: %s", raw)
	}
	r, b, _ = ts.do(admin, "POST", "/api/projects/"+id+"/members", ts.csrf(admin), map[string]string{"username": "alice"})
	expectStatus(t, "member", r, b, http.StatusNoContent)
	r, b, _ = ts.do(alice, "PUT", "/api/projects/sync", ts.csrf(alice), declaration)
	expectStatus(t, "member repeat", r, b, http.StatusOK)
	delete(declaration, "git_credential")
	r, b, _ = ts.do(alice, "PUT", "/api/projects/sync", ts.csrf(alice), declaration)
	expectStatus(t, "absent keeps", r, b, http.StatusOK)
	if b["changed"] != false || b["git_credential"] != "gitlab" || b["origin_mode"] != "gateway" {
		t.Fatalf("absent reference: %v", b)
	}
	declaration["git_credential"] = "other"
	if r, _, _ = ts.do(alice, "PUT", "/api/projects/sync", ts.csrf(alice), declaration); r.StatusCode != http.StatusForbidden {
		t.Fatalf("member changed the reference: %d", r.StatusCode)
	}
	for _, body := range []string{
		`{"name":"declared","repository":"` + gitTestOrigin + `","git_credential":"Bad"}`,
		`{"name":"declared","repository":"` + gitTestOrigin + `","git_credential":"../gitlab"}`,
		`{"name":"declared","repository":"` + gitTestOrigin + `","git_credential":1}`,
		`{"name":"declared","repository":"` + gitTestOrigin + `","git_credential":null}`,
		`{"name":"declared","repository":"` + gitTestOrigin + `","git_credential":{"token":"` + gitTokenSentinel + `"}}`,
		`{"name":"declared","repository":"","git_credential":"gitlab"}`,
		`{"name":"declared","repository":"` + gitTestOrigin + `","gitCredential":"gitlab"}`,
	} {
		w := syncRaw(t, ts, admin, body)
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), gitTokenSentinel) {
			t.Fatalf("body %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	// The declaration moves to another server: the stored credential stays bound
	// to the old repository and is no longer resolved.
	declaration["git_credential"] = "gitlab"
	declaration["repository"] = "https://elsewhere.example/group/repo.git"
	r, b, _ = ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), declaration)
	expectStatus(t, "repository change", r, b, http.StatusOK)
	if b["changed"] != true || b["git_credential_bound"] != false || b["origin_mode"] != "" {
		t.Fatalf("repository change: %v", b)
	}
	if access, err := ts.srv.store.ProjectGitAccess(ctx, id); err != nil || access.HasCredential() {
		t.Fatalf("credential resolved for another repository: %v", err)
	}
	declaration["repository"] = gitTestOrigin
	declaration["git_credential"] = ""
	r, b, _ = ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), declaration)
	expectStatus(t, "clear", r, b, http.StatusOK)
	if b["changed"] != true || b["git_credential"] != "" || b["git_credential_bound"] != false || b["origin_mode"] != "" {
		t.Fatalf("cleared reference: %v", b)
	}
}
