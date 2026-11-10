package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/gitcredential"
)

const clientGitToken = "synthetic-git-token-sentinel"

// FEATURE: Client transport for server-only project Git credentials (#173).
// SPEC: Normal session and CSRF transport; the PUT body names the four fields
// explicitly (the credential type itself encodes as redacted); responses are
// metadata only; input is validated before any request; error bodies and
// transport details never reach the caller.
// IMPLEMENTATION: Explicit wire struct, shared validators, redacted errors.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGitCredentialTransport(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "session" {
			t.Error("missing session")
		}
		if r.Method != "GET" {
			if r.Header.Get("X-CSRFToken") != "csrf" || r.Header.Get("Origin") == "" {
				t.Error("missing normal CSRF transport")
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			want := map[string]any{"names": []any{"gitlab"}}
			if r.Method == "PUT" {
				want = map[string]any{"name": "gitlab", "repository": "https://git.example.com/group/repo.git", "username": "brood-bot", "token": clientGitToken}
			}
			if !reflect.DeepEqual(body, want) {
				t.Errorf("%s body = %v", r.Method, body)
			}
		}
		fmt.Fprint(w, `{"items":[{"name":"gitlab","repository":"https://git.example.com/group/repo.git","active":true}]}`)
	}))
	defer server.Close()
	c, _ := New(server.URL)
	c.Restore(StoredSession{Session: "session", CSRF: "csrf"})
	ctx := context.Background()
	want := []gitcredential.Info{{Name: "gitlab", Repository: "https://git.example.com/group/repo.git", Active: true}}
	credential := gitcredential.Credential{Name: "gitlab", Repository: "https://git.example.com/group/repo.git", Username: "brood-bot", Token: clientGitToken}
	for name, call := range map[string]func() ([]gitcredential.Info, error){
		"list": func() ([]gitcredential.Info, error) { return c.ProjectGitCredentials(ctx, "p1") },
		"put":  func() ([]gitcredential.Info, error) { return c.PutProjectGitCredential(ctx, "p1", credential) },
		"delete": func() ([]gitcredential.Info, error) {
			return c.DeleteProjectGitCredentials(ctx, "p1", []string{"gitlab"})
		},
	} {
		got, err := call()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
	for _, request := range []string{"GET /api/projects/p1/secrets/git", "PUT /api/projects/p1/secrets/git", "DELETE /api/projects/p1/secrets/git"} {
		found := false
		for _, seen := range requests {
			found = found || seen == request
		}
		if !found {
			t.Fatalf("request %q missing from %v", request, requests)
		}
	}
}

func TestGitCredentialClientValidatesAndRedacts(t *testing.T) {
	calls := 0
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(status)
		fmt.Fprint(w, `{"detail":"`+clientGitToken+`","items":"`+clientGitToken+`"}`)
	}))
	defer server.Close()
	c, _ := New(server.URL)
	ctx := context.Background()
	for _, credential := range []gitcredential.Credential{
		{Name: "gitlab", Repository: "https://brood-bot:" + clientGitToken + "@git.example.com/group/repo.git", Username: "brood-bot", Token: clientGitToken},
		{Name: "Bad", Repository: "https://git.example.com/group/repo.git", Username: "brood-bot", Token: clientGitToken},
		{Name: "gitlab", Repository: "https://git.example.com/group/repo.git", Username: "brood-bot", Token: clientGitToken + "\n"},
	} {
		if _, err := c.PutProjectGitCredential(ctx, "p", credential); err == nil || strings.Contains(err.Error(), clientGitToken) {
			t.Fatalf("invalid credential: %v", err)
		}
	}
	if _, err := c.DeleteProjectGitCredentials(ctx, "p", nil); err == nil {
		t.Fatal("empty removal accepted")
	}
	if _, err := c.DeleteProjectGitCredentials(ctx, "p", []string{"../x"}); err == nil {
		t.Fatal("invalid removal name accepted")
	}
	if _, err := c.ProjectGitCredentials(ctx, ""); err == nil {
		t.Fatal("implicit project accepted")
	}
	if calls != 0 {
		t.Fatal("invalid input reached the server")
	}
	valid := gitcredential.Credential{Name: "gitlab", Repository: "https://git.example.com/group/repo.git", Username: "brood-bot", Token: clientGitToken}
	for _, status = range []int{200, 400, 401, 403, 422, 500} {
		_, err := c.PutProjectGitCredential(ctx, "p", valid)
		if err == nil || strings.Contains(err.Error(), clientGitToken) {
			t.Fatalf("status %d: unsafe error %v", status, err)
		}
		if status == 401 && !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("lost expiry type: %v", err)
		}
	}
}
