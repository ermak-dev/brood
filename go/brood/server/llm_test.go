package server

// FEATURE: Embedded panel management and native LLM proxy.
// SPEC: Explicit backend, fresh admin session, unsafe CSRF/Origin, no credential leakage.
// IMPLEMENTATION: ReverseProxy with separate admin and native-client gates.
// RESULT: IMPLEMENTED — contract, stream, upgrade and traversal tests pass.
// STATUS: DONE

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
)

func newLLMTestServer(t *testing.T, backend string) *testServer {
	t.Helper()
	cfg, err := LoadConfig(func(k string) string {
		return map[string]string{
			"BROOD_DB_PATH": t.TempDir() + "/brood.db", "CLIPROXY_URL": backend,
			"CLIPROXY_MANAGEMENT_KEY": "server-secret",
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(nil, cfg, Options{Provisioner: newFakeProvisioner()})
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{t: t, srv: srv, http: httptest.NewServer(srv.Handler())}
	t.Cleanup(func() { ts.http.Close(); srv.Close() })
	return ts
}

func TestLLMManagement(t *testing.T) {
	calls := make(chan *http.Request, 16)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- r.Clone(r.Context())
		w.Header().Set("Set-Cookie", "backend=secret")
		if r.URL.Query().Get("redirect") != "" {
			w.Header().Set("Location", "https://outside.invalid")
			w.WriteHeader(302)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer backend.Close()
	ts := newLLMTestServer(t, backend.URL)
	ts.bootstrapAdmin("root", "root-password-1")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	for _, p := range []string{"/v8/management/config", "/v0/resource/plugins/demo/index.html", "/v0/management/demo"} {
		resp, data, _ := ts.do(newClient(t), "GET", p, "", nil)
		expectStatus(t, "anonymous management", resp, data, 401)
		req, _ := http.NewRequest("GET", ts.http.URL+p+"?name=a%2Fb", nil)
		req.Header.Set("Authorization", "Bearer browser-key")
		req.Header.Set("X-Management-Key", "browser-key")
		req.Header.Set("X-Api-Key", "browser-key")
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Set-Cookie") != "" {
			t.Fatalf("management status/headers: %d %v", resp.StatusCode, resp.Header)
		}
		got := <-calls
		if got.Host != req.URL.Host || got.Header.Get("X-Api-Key") != "" {
			t.Fatalf("edge Host/auth contract changed: %s %v", got.Host, got.Header)
		}
		if got.URL.RequestURI() != p+"?name=a%2Fb" || got.Header.Get("Authorization") != "Bearer server-secret" || got.Header.Get("Cookie") != "" || got.Header.Get("X-Management-Key") != "" {
			t.Fatalf("upstream request leaked or changed: %s %v", got.URL, got.Header)
		}
	}
	resp, data, _ := ts.do(admin, "PUT", "/v8/management/config", "", map[string]bool{"enabled": true})
	expectStatus(t, "missing CSRF", resp, data, 403)
	req, _ := http.NewRequest("PUT", ts.http.URL+"/v8/management/config", strings.NewReader(`{}`))
	req.Header.Set(auth.HeaderCSRF, ts.csrf(admin))
	resp, err := admin.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("missing Origin: %d", resp.StatusCode)
	}
	resp, data, _ = ts.do(admin, "PUT", "/v8/management/config", ts.csrf(admin), map[string]bool{"enabled": true})
	expectStatus(t, "valid mutation", resp, data, 200)
	<-calls
	resp, data, _ = ts.do(admin, "GET", "/v8/management/config?redirect=1", "", nil)
	expectStatus(t, "upstream redirect blocked", resp, data, 502)
	<-calls
	resp, data, _ = ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]any{"username": "alice", "password": "alice-password-1"})
	expectStatus(t, "create user", resp, data, 201)
	alice := newClient(t)
	ts.login(alice, "alice", "alice-password-1")
	resp, data, _ = ts.do(alice, "GET", "/v8/management/config", "", nil)
	expectStatus(t, "non-admin", resp, data, 403)
	ts.do(admin, "POST", "/api/auth/logout", ts.csrf(admin), nil)
	resp, data, _ = ts.do(admin, "GET", "/v8/management/config", "", nil)
	expectStatus(t, "revoked session", resp, data, 401)
	select {
	case r := <-calls:
		t.Fatalf("denied request reached backend: %s", r.URL)
	default:
	}
}

func TestLLMPublic(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer native-key" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Management-Key") != "" {
			t.Errorf("public headers: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: ok\n\n")
		w.(http.Flusher).Flush()
	}))
	defer backend.Close()
	ts := newLLMTestServer(t, backend.URL)
	for _, p := range []string{"/v1/responses", "/v1beta/models", "/openai/v1/chat/completions", "/backend-api/codex/responses", "/anthropic/callback", "/codex/callback", "/antigravity/callback", "/devin/callback", "/callback"} {
		req, _ := http.NewRequest("POST", ts.http.URL+p, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer native-key")
		req.Header.Set("Cookie", "brood_session=invalid")
		req.Header.Set("X-Management-Key", "browser")
		resp, err := newClient(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(b) != "data: ok\n\n" {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, b)
		}
	}
}

func TestLLMUnconfigured(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "root-password-1")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	for _, p := range []string{"/v1/models", "/v8/management/config"} {
		resp, data, raw := ts.do(admin, "GET", p, "", nil)
		expectStatus(t, p, resp, data, 503)
		if !strings.Contains(raw, "CLIPROXY_URL") {
			t.Fatalf("missing configuration reason: %s", raw)
		}
	}
}

func TestLLMConfig(t *testing.T) {
	for _, raw := range []string{"http://user:password@host", "file:///tmp/socket", "http://host/prefix", "http://host?key=x", "http://host/#fragment"} {
		_, err := LoadConfig(func(k string) string {
			if k == "CLIPROXY_URL" {
				return raw
			}
			return ""
		})
		if err == nil {
			t.Errorf("accepted invalid CLIPROXY_URL %q", raw)
		}
	}
}
