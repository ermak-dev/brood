package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brood.dev/go/brood/projectsecret"
)

// FEATURE: Project secret API exposes metadata and write-only mutations.
// SPEC: Normal authenticated transport; base64 file bodies; no error-body leakage.
// IMPLEMENTATION: Typed metadata methods reuse call and redact untrusted response errors.
// RESULT: IMPLEMENTED — request contracts, validation and typed expiry pass with race checks.
// STATUS: DONE
func TestSecretTransport(t *testing.T) {
	var methods, paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		if r.Method != "GET" {
			if r.Header.Get("X-CSRFToken") != "csrf" || r.Header.Get("Origin") == "" {
				t.Error("missing normal CSRF transport")
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "files") && string(body["files"]) != `[{"name":"key","data":"AP8="}]` {
				t.Errorf("file wire body %s", body["files"])
			}
			if r.Method == "DELETE" && string(body["names"]) != `["key"]` {
				t.Error("missing names body")
			}
		}
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "session" {
			t.Error("missing session")
		}
		fmt.Fprint(w, `{"revision":3,"files":[{"name":"key","size":2}],"vars":["TOKEN"]}`)
	}))
	defer server.Close()
	c, _ := New(server.URL)
	c.Restore(StoredSession{Session: "session", CSRF: "csrf"})
	ctx := context.Background()
	meta, err := c.ProjectSecrets(ctx, "p1")
	if err != nil || meta.Revision != 3 || meta.Files[0].Size != 2 {
		t.Fatalf("metadata %+v %v", meta, err)
	}
	if _, err = c.PutProjectSecretFiles(ctx, "p1", []projectsecret.File{{Name: "key", Data: []byte{0, 255}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.PutProjectSecretVars(ctx, "p1", map[string]string{"TOKEN": "dummy"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.DeleteProjectSecretFiles(ctx, "p1", []string{"key"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.DeleteProjectSecretVars(ctx, "p1", []string{"key"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "GET,PUT,PUT,DELETE,DELETE" || strings.Join(paths, ",") != "/api/projects/p1/secrets,/api/projects/p1/secrets/files,/api/projects/p1/secrets/vars,/api/projects/p1/secrets/files,/api/projects/p1/secrets/vars" {
		t.Fatalf("requests %v %v", methods, paths)
	}
}

func TestSecretErrorsAreRedacted(t *testing.T) {
	for _, status := range []int{400, 401, 403, 500, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"revision":"dummy-sensitive-value"}`)
			}))
			defer server.Close()
			c, _ := New(server.URL)
			_, err := c.PutProjectSecretVars(context.Background(), "p", map[string]string{"TOKEN": "dummy-sensitive-value"})
			if err == nil || strings.Contains(err.Error(), "dummy-sensitive-value") {
				t.Fatalf("unsafe error %v", err)
			}
			if status == 401 && !errors.Is(err, ErrSessionExpired) {
				t.Fatalf("lost expiry type: %v", err)
			}
		})
	}
}

func TestSecretClientValidatesBeforeRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, `{}`) }))
	defer server.Close()
	c, _ := New(server.URL)
	ctx := context.Background()
	if _, err := c.PutProjectSecretVars(ctx, "p", map[string]string{"ANTHROPIC_API_KEY": "dummy"}); err == nil {
		t.Error("reserved variable accepted")
	}
	if _, err := c.PutProjectSecretFiles(ctx, "p", []projectsecret.File{{Name: "../key"}}); err == nil {
		t.Error("traversal accepted")
	}
	if calls != 0 {
		t.Fatal("invalid batch sent")
	}
}
