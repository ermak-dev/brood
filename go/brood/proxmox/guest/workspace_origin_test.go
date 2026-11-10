//go:build linux

package guest

// FEATURE: Workspace origin fetched by the root supervisor (#147).
// SPEC: FetchWorkspaceOrigin GETs /api/proxmox/workspace/{uid}/{generation} on
// the relay host with the bootstrap token and the pinned CA, without proxy or
// redirects, bounded like the project secrets fetch. The body is exactly
// {"origin","mode"}; the pair passes the local canonical check, and a direct
// origin must be an approved guest host. Errors carry neither the token nor
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

func workspaceOriginJSON(origin, mode string) string {
	data, _ := json.Marshal(map[string]string{"origin": origin, "mode": mode})
	return string(data)
}

func TestWorkspaceOriginFetch(t *testing.T) {
	server := newWorkspaceOriginServer(t)
	cfg := server.config()
	hosts := []string{"github.com"}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, want := range []WorkspaceOrigin{
		{Origin: cloneTestOrigin, Mode: "gateway"},
		{Origin: "https://github.com/owner/repo", Mode: "direct"},
		{},
	} {
		server.serve(workspaceOriginJSON(want.Origin, want.Mode), http.StatusOK)
		got, err := FetchWorkspaceOrigin(context.Background(), cfg, hosts)
		if err != nil || got != want {
			t.Fatalf("fetch %+v: %+v %v", want, got, err)
		}
	}
	// Идентичность (#249): необязательный объект ровно {name, email} при origin.
	withIdentity := `{"origin":"` + cloneTestOrigin + `","mode":"gateway","gitIdentity":{"name":"brood worker2","email":"worker2@brood.corp.example"}}`
	server.serve(withIdentity, http.StatusOK)
	wantIdentity := WorkspaceOrigin{Origin: cloneTestOrigin, Mode: "gateway", GitIdentity: gitidentity.Identity{Name: "brood worker2", Email: "worker2@brood.corp.example"}}
	if got, err := FetchWorkspaceOrigin(context.Background(), cfg, hosts); err != nil || got != wantIdentity {
		t.Fatalf("fetch with identity: %+v %v", got, err)
	}
	bad := map[string]string{
		"identity without origin": `{"origin":"","mode":"","gitIdentity":{"name":"brood worker2","email":"worker2@brood.corp.example"}}`,
		"identity angle":          `{"origin":"` + cloneTestOrigin + `","mode":"gateway","gitIdentity":{"name":"brood <worker2>","email":"worker2@brood.corp.example"}}`,
		"identity bad email":      `{"origin":"` + cloneTestOrigin + `","mode":"gateway","gitIdentity":{"name":"brood worker2","email":"private-token"}}`,
		"identity extra field":    `{"origin":"` + cloneTestOrigin + `","mode":"gateway","gitIdentity":{"name":"brood worker2","email":"worker2@brood.corp.example","token":"private-token"}}`,
		"identity missing email":  `{"origin":"` + cloneTestOrigin + `","mode":"gateway","gitIdentity":{"name":"brood worker2"}}`,
		"identity null":           `{"origin":"` + cloneTestOrigin + `","mode":"gateway","gitIdentity":null}`,
		"identity null field":     `{"origin":"` + cloneTestOrigin + `","mode":"gateway","gitIdentity":{"name":null,"email":"worker2@brood.corp.example"}}`,
		"identity string":         `{"origin":"` + cloneTestOrigin + `","mode":"gateway","gitIdentity":"brood worker2 <worker2@brood.corp.example>"}`,
		"identity twice":          `{"origin":"` + cloneTestOrigin + `","mode":"gateway","gitIdentity":{"name":"brood worker2","email":"worker2@brood.corp.example"},"gitIdentity":{"name":"brood worker2","email":"worker2@brood.corp.example"}}`,
		"origin without mode":     workspaceOriginJSON(cloneTestOrigin, ""),
		"mode without origin":     workspaceOriginJSON("", "gateway"),
		"unknown mode":            workspaceOriginJSON(cloneTestOrigin, "ssh"),
		"plain HTTP":              workspaceOriginJSON("http://git.example.org/repo.git", "gateway"),
		"userinfo":                workspaceOriginJSON("https://private-token@git.example.org/repo.git", "gateway"),
		"query":                   workspaceOriginJSON("https://git.example.org/repo.git?private-token", "gateway"),
		"fragment":                workspaceOriginJSON("https://git.example.org/repo.git#private-token", "gateway"),
		"empty fragment":          workspaceOriginJSON("https://git.example.org/repo.git#", "gateway"),
		"port":                    workspaceOriginJSON("https://git.example.org:8443/repo.git", "gateway"),
		"IP literal":              workspaceOriginJSON("https://192.0.2.1/repo.git", "gateway"),
		"IPv6 literal":            workspaceOriginJSON("https://[2001:db8::1]/repo.git", "gateway"),
		"numeric host":            workspaceOriginJSON("https://1.2.3/repo.git", "gateway"),
		"uppercase host":          workspaceOriginJSON("https://Git.example.org/repo.git", "gateway"),
		"trailing dot":            workspaceOriginJSON("https://git.example.org./repo.git", "gateway"),
		"trailing slash":          workspaceOriginJSON("https://git.example.org/repo.git/", "gateway"),
		"empty path":              workspaceOriginJSON("https://git.example.org", "gateway"),
		"root path":               workspaceOriginJSON("https://git.example.org/", "gateway"),
		"control character":       workspaceOriginJSON("https://git.example.org/re\tpo.git", "gateway"),
		"too long":                workspaceOriginJSON("https://git.example.org/"+strings.Repeat("a", 1024), "gateway"),
		"unapproved direct":       workspaceOriginJSON("https://git.example.org/repo.git", "direct"),
		"unknown field":           `{"origin":"","mode":"","token":"private-token"}`,
		"missing mode":            `{"origin":""}`,
		"missing origin":          `{"mode":""}`,
		"empty object":            `{}`,
		"null field":              `{"origin":null,"mode":""}`,
		"wrong type":              `{"origin":1,"mode":""}`,
		"trailing document":       workspaceOriginJSON("", "") + `{"private-token":1}`,
		"array":                   `["private-token"]`,
		"not JSON":                `private-token`,
		"empty body":              ``,
		"oversized":               `{"origin":"","mode":""}` + strings.Repeat(" ", 4<<10),
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			server.serve(body, http.StatusOK)
			got, err := FetchWorkspaceOrigin(context.Background(), cfg, hosts)
			if err == nil || got != (WorkspaceOrigin{}) || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), cfg.Token) {
				t.Fatalf("not safely rejected: %+v %v", got, err)
			}
		})
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusServiceUnavailable} {
		server.serve(workspaceOriginJSON(cloneTestOrigin, "gateway"), status)
		if got, err := FetchWorkspaceOrigin(context.Background(), cfg, hosts); err == nil || got != (WorkspaceOrigin{}) {
			t.Fatalf("status %d accepted: %+v", status, got)
		}
	}
	redirected := false
	sink := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer sink.Close()
	server.mu.Lock()
	server.redirect = sink.URL
	server.mu.Unlock()
	if _, err := FetchWorkspaceOrigin(context.Background(), cfg, hosts); err == nil || redirected {
		t.Fatalf("redirect followed: %v %v", err, redirected)
	}
	server.mu.Lock()
	server.redirect = ""
	server.mu.Unlock()
	server.serve(workspaceOriginJSON(cloneTestOrigin, "gateway"), http.StatusOK)
	untrusted := cfg
	untrusted.CAPEM = nil
	if _, err := FetchWorkspaceOrigin(context.Background(), untrusted, hosts); err == nil {
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
		if _, err := FetchWorkspaceOrigin(context.Background(), c, hosts); err == nil {
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
	server.serve(workspaceOriginJSON(cloneTestOrigin, "gateway"), http.StatusOK)
	if got, err := RefreshWorkspaceOrigin(context.Background(), server.config(), []string{"github.com"}); err == nil || got != (WorkspaceOrigin{}) {
		t.Fatalf("unprivileged refresh: %+v %v", got, err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.requests != 0 {
		t.Fatal("unprivileged refresh contacted the server")
	}
}
