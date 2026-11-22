//go:build linux

package guest

// FEATURE: Workspace origin fetched by the root supervisor (#147).
// SPEC: fetchWorkspace GETs /api/proxmox/workspace/{uid}/{generation} on
// the relay host with the bootstrap token and the pinned CA, without proxy or
// redirects, bounded like the project secrets fetch. The body is exactly
// {"origin","sshPrivateKey"} with optional metadata. Errors carry neither the token nor
// the response. Only guest root may stage the result for the app wrapper.

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/gitidentity"
	"brood.dev/go/brood/proxmox/relay"
)

// workspaceOriginServer serves body (or the status) as the server's workspace
// route and records every request it receives.
type workspaceOriginServer struct {
	*httptest.Server
	mu       sync.Mutex
	body     string
	status   int
	redirect string
	requests int
}

func newWorkspaceOriginServer(t *testing.T) *workspaceOriginServer {
	t.Helper()
	s := &workspaceOriginServer{status: http.StatusOK}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		body, status, redirect := s.body, s.status, s.redirect
		s.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/api/proxmox/workspace/actor-1/3" || r.URL.RawQuery != "" || len(r.Header.Values("Authorization")) != 1 || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 43) || r.Header.Get("Cookie") != "" || r.ContentLength > 0 {
			t.Error("incorrect workspace origin request")
		}
		if redirect != "" {
			http.Redirect(w, r, redirect, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *workspaceOriginServer) serve(body string, status int) {
	s.mu.Lock()
	s.body, s.status = body, status
	s.mu.Unlock()
}

func (s *workspaceOriginServer) config() relay.ClientConfig {
	return relay.ClientConfig{URL: strings.Replace(s.URL, "https:", "wss:", 1) + "/api/proxmox/guest/actor-1/3", ActorUID: "actor-1", Generation: 3, Token: strings.Repeat("a", 43), CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})}
}

func workspaceOriginJSON(origin string) string {
	data, _ := json.Marshal(map[string]string{"origin": origin, "sshPrivateKey": ""})
	return string(data)
}

func TestWorkspaceOriginFetch(t *testing.T) {
	server := newWorkspaceOriginServer(t)
	cfg := server.config()
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, want := range []WorkspaceOrigin{
		{Origin: cloneTestOrigin},
		{Origin: "https://github.com/owner/repo"},
		{},
	} {
		server.serve(workspaceOriginJSON(want.Origin), http.StatusOK)
		got, err := fetchWorkspace(context.Background(), cfg)
		if err != nil || got.WorkspaceOrigin != want {
			t.Fatalf("fetch %+v: %+v %v", want, got, err)
		}
	}
	// Идентичность (#249): необязательный объект ровно {name, email} при origin.
	withIdentity := `{"origin":"` + cloneTestOrigin + `","sshPrivateKey":"","gitIdentity":{"name":"brood worker2","email":"worker2@brood.corp.example"}}`
	server.serve(withIdentity, http.StatusOK)
	wantIdentity := WorkspaceOrigin{Origin: cloneTestOrigin, GitIdentity: gitidentity.Identity{Name: "brood worker2", Email: "worker2@brood.corp.example"}}
	if got, err := fetchWorkspace(context.Background(), cfg); err != nil || got.WorkspaceOrigin != wantIdentity {
		t.Fatalf("fetch with identity: %+v %v", got, err)
	}
	bad := map[string]string{
		"identity without origin": `{"origin":"","sshPrivateKey":"","gitIdentity":{"name":"brood worker2","email":"worker2@brood.corp.example"}}`,
		"identity angle":          `{"origin":"` + cloneTestOrigin + `","sshPrivateKey":"","gitIdentity":{"name":"brood <worker2>","email":"worker2@brood.corp.example"}}`,
		"identity bad email":      `{"origin":"` + cloneTestOrigin + `","sshPrivateKey":"","gitIdentity":{"name":"brood worker2","email":"private-token"}}`,
		"identity extra field":    `{"origin":"` + cloneTestOrigin + `","sshPrivateKey":"","gitIdentity":{"name":"brood worker2","email":"worker2@brood.corp.example","token":"private-token"}}`,
		"identity missing email":  `{"origin":"` + cloneTestOrigin + `","sshPrivateKey":"","gitIdentity":{"name":"brood worker2"}}`,
		"identity null":           `{"origin":"` + cloneTestOrigin + `","sshPrivateKey":"","gitIdentity":null}`,
		"identity null field":     `{"origin":"` + cloneTestOrigin + `","sshPrivateKey":"","gitIdentity":{"name":null,"email":"worker2@brood.corp.example"}}`,
		"identity string":         `{"origin":"` + cloneTestOrigin + `","sshPrivateKey":"","gitIdentity":"brood worker2 <worker2@brood.corp.example>"}`,
		"identity twice":          `{"origin":"` + cloneTestOrigin + `","sshPrivateKey":"","gitIdentity":{"name":"brood worker2","email":"worker2@brood.corp.example"},"gitIdentity":{"name":"brood worker2","email":"worker2@brood.corp.example"}}`,
		"plain HTTP":              workspaceOriginJSON("http://git.example.org/repo.git"),
		"userinfo":                workspaceOriginJSON("https://private-token@git.example.org/repo.git"),
		"query":                   workspaceOriginJSON("https://git.example.org/repo.git?private-token"),
		"fragment":                workspaceOriginJSON("https://git.example.org/repo.git#private-token"),
		"empty fragment":          workspaceOriginJSON("https://git.example.org/repo.git#"),
		"port":                    workspaceOriginJSON("https://git.example.org:8443/repo.git"),
		"IP literal":              workspaceOriginJSON("https://192.0.2.1/repo.git"),
		"IPv6 literal":            workspaceOriginJSON("https://[2001:db8::1]/repo.git"),
		"numeric host":            workspaceOriginJSON("https://1.2.3/repo.git"),
		"uppercase host":          workspaceOriginJSON("https://Git.example.org/repo.git"),
		"trailing dot":            workspaceOriginJSON("https://git.example.org./repo.git"),
		"trailing slash":          workspaceOriginJSON("https://git.example.org/repo.git/"),
		"empty path":              workspaceOriginJSON("https://git.example.org"),
		"root path":               workspaceOriginJSON("https://git.example.org/"),
		"control character":       workspaceOriginJSON("https://git.example.org/re\tpo.git"),
		"too long":                workspaceOriginJSON("https://git.example.org/" + strings.Repeat("a", 1024)),
		"unknown field":           `{"origin":"","sshPrivateKey":"","token":"private-token"}`,
		"missing SSH private key": `{"origin":""}`,
		"missing origin":          `{"sshPrivateKey":""}`,
		"empty object":            `{}`,
		"null field":              `{"origin":null,"sshPrivateKey":""}`,
		"wrong type":              `{"origin":1,"sshPrivateKey":""}`,
		"trailing document":       workspaceOriginJSON("") + `{"private-token":1}`,
		"array":                   `["private-token"]`,
		"not JSON":                `private-token`,
		"empty body":              ``,
		"oversized":               `{"origin":"","sshPrivateKey":""}` + strings.Repeat(" ", workspacePayloadLimit),
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			server.serve(body, http.StatusOK)
			got, err := fetchWorkspace(context.Background(), cfg)
			if err == nil || got != (workspacePayload{}) || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), cfg.Token) {
				t.Fatalf("not safely rejected: %+v %v", got, err)
			}
		})
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusServiceUnavailable} {
		server.serve(workspaceOriginJSON(cloneTestOrigin), status)
		if got, err := fetchWorkspace(context.Background(), cfg); err == nil || got != (workspacePayload{}) {
			t.Fatalf("status %d accepted: %+v", status, got)
		}
	}
	redirected := false
	sink := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer sink.Close()
	server.mu.Lock()
	server.redirect = sink.URL
	server.mu.Unlock()
	if _, err := fetchWorkspace(context.Background(), cfg); err == nil || redirected {
		t.Fatalf("redirect followed: %v %v", err, redirected)
	}
	server.mu.Lock()
	server.redirect = ""
	server.mu.Unlock()
	server.serve(workspaceOriginJSON(cloneTestOrigin), http.StatusOK)
	untrusted := cfg
	untrusted.CAPEM = nil
	if _, err := fetchWorkspace(context.Background(), untrusted); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
	server.mu.Lock()
	before := server.requests
	server.mu.Unlock()
	for name, broken := range map[string]func(*relay.ClientConfig){
		"scheme":     func(c *relay.ClientConfig) { c.URL = strings.Replace(c.URL, "wss:", "https:", 1) },
		"path":       func(c *relay.ClientConfig) { c.URL = strings.Replace(c.URL, "actor-1/3", "actor-2/3", 1) },
		"token":      func(c *relay.ClientConfig) { c.Token = "short" },
		"generation": func(c *relay.ClientConfig) { c.Generation = 0 },
		"uid":        func(c *relay.ClientConfig) { c.ActorUID = "../actor-1" },
	} {
		c := cfg
		broken(&c)
		if _, err := fetchWorkspace(context.Background(), c); err == nil {
			t.Fatalf("invalid bootstrap %s accepted", name)
		}
	}
	server.mu.Lock()
	after := server.requests
	server.mu.Unlock()
	if after != before {
		t.Fatal("invalid bootstrap identity reached the server")
	}
}

func TestRefreshWorkspaceOriginRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged test process")
	}
	server := newWorkspaceOriginServer(t)
	server.serve(workspaceOriginJSON(cloneTestOrigin), http.StatusOK)
	if err := RefreshWorkspace(context.Background(), server.config()); err == nil {
		t.Fatal("unprivileged refresh succeeded")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.requests != 0 {
		t.Fatal("unprivileged refresh contacted the server")
	}
}
