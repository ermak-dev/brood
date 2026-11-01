package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
)

// FEATURE: Supervisor-authenticated model access for managed actor incarnations.
// SPEC: Only models GET and messages POST; current guest token, fixed backend,
// server-only API key, sanitized headers/errors, bounded requests and streaming.
// IMPLEMENTATION: Dedicated machine handler with the existing guest authorizer
// and a separate fixed-origin reverse proxy; browser/public LLM gates unchanged.
// RESULT: IMPLEMENTED — fake backend verifies authorization and HTTP isolation.
// STATUS: DONE
func managedLLMSetup(t *testing.T, backend, key string) (*substrateServer, *http.Client, string, string, string) {
	t.Helper()
	cfg, err := loadProxmox(runtimeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	ts := newSubstrateServer(t, newFakeAteapi(), func(c *Config) { c.proxmox = cfg.proxmox; c.CLIProxyURL = backend; c.CLIProxyAPIKey = key })
	ts.srv.managed.stopReconcile()
	pve := &managedFakePVE{server: ts.srv}
	ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return pve, nil }
	if ts.srv.managed.hub != nil {
		ts.srv.managed.hub.Close()
	}
	ts.srv.managed.hub = &managedFakeRelay{connected: true}
	admin, alice, _, id := actorsSetup(t, ts.testServer)
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-pools/brood-proxmox/activate", ts.csrf(admin), nil)
	expectStatus(t, "activate", r, b, 201)
	r, b, _ = ts.do(alice, "POST", "/api/swarms/"+id+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "model"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	uid := b["metadata"].(map[string]any)["uid"].(string)
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	return ts, alice, "/api/swarms/" + id + "/actors/model", "/api/proxmox/llm/" + uid + "/1", pve.spec.Env["BROOD_GUEST_RELAY_TOKEN"]
}

func llmRequest(t *testing.T, h http.Handler, method, path, token string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestManagedLLMAuthRoutingAndHeaders(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/models" && r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer server-model-key" {
			t.Errorf("wrong injected credential")
		}
		for _, key := range []string{"Cookie", "X-Api-Key", "Api-Key", "X-Management-Key", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-CSRFToken", "X-Brood-User", "Origin", "Referer", "X-Goog-Api-Key"} {
			if r.Header.Get(key) != "" {
				t.Errorf("leaked header %s", key)
			}
		}
		io.Copy(io.Discard, r.Body)
		if len(r.Trailer) != 0 {
			t.Errorf("untrusted trailers reached backend: %v", r.Trailer)
		}
		if r.URL.RawQuery != "" {
			t.Error("query leaked")
		}
		w.Header().Set("Set-Cookie", "operator=secret")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[]}`)
	}))
	defer backend.Close()
	ts, alice, actor, base, token := managedLLMSetup(t, backend.URL, "server-model-key")
	h := ts.srv.Handler()
	req := httptest.NewRequest("POST", base+"/v1/messages", strings.NewReader(`{"model":"fake","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	for _, key := range []string{"Cookie", "X-Api-Key", "Api-Key", "X-Management-Key", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-CSRFToken", "X-Brood-User", "Origin", "Referer", "X-Goog-Api-Key"} {
		req.Header.Set(key, "untrusted")
	}
	req.Trailer = http.Header{"X-Management-Key": []string{"untrusted-trailer"}}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("proxy status=%d headers=%v body=%s", w.Code, w.Header(), w.Body)
	}
	if got := llmRequest(t, h, "GET", base+"/v1/models", token, nil); got.Code != 200 {
		t.Fatalf("models: %d %s", got.Code, got.Body)
	}
	for _, bad := range []string{"", "wrong", ts.srv.managed.registry.relaySecret, "server-model-key"} {
		got := llmRequest(t, h, "GET", base+"/v1/models", bad, nil)
		if got.Code != 401 || strings.Contains(got.Body.String(), bad) && bad != "" {
			t.Errorf("auth status=%d body=%s", got.Code, got.Body)
		}
	}
	r := httptest.NewRequest("GET", base+"/v1/models", nil)
	origin, _ := url.Parse(ts.http.URL)
	for _, cookie := range alice.Jar.Cookies(origin) {
		r.AddCookie(cookie)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("cookie authenticated: %d", w.Code)
	}
	r.Header.Add("Authorization", "Bearer "+token)
	r.Header.Add("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("duplicate authorization: %d", w.Code)
	}
	before := calls.Load()
	for _, path := range []string{base + "/v0/management", base + "/v1/messages/", base + "/v1/../models", base + "/v1/%6dodels", base + "/v1/models?url=http://elsewhere", base + "/v1/models?", base + "/v1/models#fragment", strings.Replace(base, "/1", "/01", 1) + "/v1/models", strings.Replace(base, "/1", "/0", 1) + "/v1/models", "/api/proxmox/llm/bad.uid/1/v1/models", "/api/proxmox/llm//1/v1/models"} {
		got := llmRequest(t, h, "GET", path, token, nil)
		if got.Code != 404 {
			t.Errorf("path %q: %d %s", path, got.Code, got.Body)
		}
	}
	for _, method := range []string{"HEAD", "PUT", "DELETE", "OPTIONS", "POST"} {
		got := llmRequest(t, h, method, base+"/v1/models", token, nil)
		if got.Code != 405 {
			t.Errorf("method %s: %d", method, got.Code)
		}
	}
	if calls.Load() != before {
		t.Fatal("denied request reached backend")
	}
	// Stop and reset intent must revoke the old incarnation before reconciliation.
	resp, b, _ := ts.do(alice, "POST", actor, ts.csrf(alice), map[string]string{"action": "stop"})
	expectStatus(t, "stop", resp, b, 200)
	if got := llmRequest(t, h, "GET", base+"/v1/models", token, nil); got.Code != 401 {
		t.Fatalf("stopped token: %d", got.Code)
	}
	for range 2 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	resp, b, _ = ts.do(alice, "POST", actor, ts.csrf(alice), map[string]string{"action": "reset"})
	expectStatus(t, "reset", resp, b, 200)
	if got := llmRequest(t, h, "GET", base+"/v1/models", token, nil); got.Code != 401 {
		t.Fatalf("reset token: %d", got.Code)
	}
}

func TestManagedLLMRetiredGenerationDisabledAndBodyLimit(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, `{}`) }))
	defer backend.Close()
	ts, _, _, base, token := managedLLMSetup(t, backend.URL, "key")
	for _, path := range []string{strings.Replace(base, "/1", "/2", 1), strings.Replace(base, "/1", "/9223372036854775808", 1)} {
		if got := llmRequest(t, ts.srv.Handler(), "GET", path+"/v1/models", token, nil); got.Code == 200 {
			t.Fatal("retired/malformed generation accepted")
		}
	}
	uid := strings.Split(strings.TrimPrefix(base, managedLLMPrefix), "/")[0]
	futureToken := guestToken(ts.srv.managed.registry.relaySecret, uid, 2)
	if got := llmRequest(t, ts.srv.Handler(), "GET", strings.Replace(base, "/1", "/2", 1)+"/v1/models", futureToken, nil); got.Code != 401 {
		t.Fatalf("valid HMAC for inactive incarnation: %d", got.Code)
	}
	if got := llmRequest(t, ts.srv.Handler(), "GET", base+"/v1/models", token, strings.NewReader("body")); got.Code != 400 {
		t.Fatalf("models body: %d", got.Code)
	}
	got := llmRequest(t, ts.srv.Handler(), "POST", base+"/v1/messages", token, strings.NewReader(strings.Repeat("x", (8<<20)+1)))
	if got.Code != 413 || calls.Load() != 0 {
		t.Fatalf("body bound: %d calls=%d", got.Code, calls.Load())
	}
	for _, cfg := range []struct{ url, key string }{{backend.URL, ""}, {"", "key"}} {
		disabled, _, _, path, tok := managedLLMSetup(t, cfg.url, cfg.key)
		got = llmRequest(t, disabled.srv.Handler(), "GET", path+"/v1/models", tok, nil)
		if got.Code != 503 {
			t.Fatalf("disabled status %d", got.Code)
		}
	}
}

func TestManagedLLMRedirectAndStreamingCancellation(t *testing.T) {
	var redirect atomic.Bool
	started := make(chan struct{})
	canceled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirect.Load() {
			w.Header().Set("Location", "https://example.invalid/secret")
			w.Header().Set("Set-Cookie", "bad=value")
			w.WriteHeader(307)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer backend.Close()
	ts, _, _, base, token := managedLLMSetup(t, backend.URL, "key")
	redirect.Store(true)
	got := llmRequest(t, ts.srv.Handler(), "POST", base+"/v1/messages", token, strings.NewReader(`{}`))
	if got.Code != 502 || got.Header().Get("Location") != "" || got.Header().Get("Set-Cookie") != "" {
		t.Fatalf("redirect: %d %v", got.Code, got.Header())
	}
	redirect.Store(false)
	edge := httptest.NewServer(ts.srv.Handler())
	defer edge.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", edge.URL+base+"/v1/messages", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("SSE not flushed: %q %v", line, err)
	}
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not canceled")
	}
}

func TestManagedLLMConfigSecret(t *testing.T) {
	key := "managed-model-secret"
	cfg, err := LoadConfig(func(k string) string {
		if k == "CLIPROXY_API_KEY" {
			return "'" + key + "'"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CLIProxyAPIKey != key {
		t.Fatal("API key not loaded")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{string(data), fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(s, key) {
			t.Fatal("config disclosed API key")
		}
	}
	for _, value := range []string{"bad\nheader", "bad\rheader", "bad\x00header", "bad\x7fheader", "bad\theader", "bad header", "badé"} {
		cfg.CLIProxyAPIKey = value
		if err := cfg.validateAdapters(); err == nil || !strings.Contains(err.Error(), "CLIPROXY_API_KEY") || strings.Contains(err.Error(), value) {
			t.Fatalf("unsafe credential validation: %v", err)
		}
	}
}
