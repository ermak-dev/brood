package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/store"
)

// gitUpstream is a TLS Git server for git.example.com: it records every
// request, requires the installation credential and adds headers the gateway
// must drop. inner serves authenticated requests.
type gitUpstream struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []gitUpstreamRequest
	inner    http.Handler
	token    string
}

type gitUpstreamRequest struct {
	method, path, query string
	header              http.Header
}

func newGitUpstream(t *testing.T, ts *substrateServer, inner http.Handler) *gitUpstream {
	t.Helper()
	cert, caPEM := selfSignFake(t, "git.example.com")
	u := &gitUpstream{inner: inner, token: gitTokenSentinel}
	u.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.requests = append(u.requests, gitUpstreamRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone()})
		handler, token := u.inner, u.token
		u.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "upstream", Value: "cookie-sentinel"})
		w.Header().Set("X-Upstream-Private", "private-header-sentinel")
		user, password, ok := r.BasicAuth()
		if !ok || user != "brood-bot" || password != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="upstream"`)
			http.Error(w, "upstream unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	u.server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	u.server.StartTLS()
	t.Cleanup(u.server.Close)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("upstream CA")
	}
	address := u.server.Listener.Addr().String()
	ts.srv.git.transport.TLSClientConfig.RootCAs = pool
	ts.srv.git.transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "git.example.com:443" {
			return nil, errors.New("unexpected upstream address")
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, address)
	}
	return u
}

func (u *gitUpstream) seen() []gitUpstreamRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]gitUpstreamRequest(nil), u.requests...)
}

func (u *gitUpstream) setInner(handler http.Handler) {
	u.mu.Lock()
	u.inner = handler
	u.mu.Unlock()
}

// rotate makes the upstream reject the installation credential the gateway
// holds, without mutating server state shared with request goroutines.
func (u *gitUpstream) rotate(token string) {
	u.mu.Lock()
	u.token = token
	u.mu.Unlock()
}

func gitAdvertisement() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "advertisement")
	})
}

type gitGatewayFixture struct {
	ts       *substrateServer
	alice    *http.Client
	swarm    string
	project  string
	actor    store.ManagedActor
	token    string
	upstream *gitUpstream
}

// gitGatewaySetup starts the gateway actor "clone" (origin = project
// repository) and gives the direct-mode actor "direct" in the same swarm a
// durable start intent. The fake PVE models one CT, so only "clone" runs there.
func gitGatewaySetup(t *testing.T) gitGatewayFixture {
	t.Helper()
	return gitGatewaySetupWith(t, nil)
}

// gitGatewaySetupWith runs configure after the project repository is selected
// and before any actor is created.
func gitGatewaySetupWith(t *testing.T, configure func(ts *substrateServer, project string)) gitGatewayFixture {
	t.Helper()
	ts, alice, swarm, _, hub := managedOriginSetup(t, gitTestOrigin)
	ctx := context.Background()
	sw, err := ts.srv.store.GetSwarm(ctx, swarm)
	if err != nil {
		t.Fatal(err)
	}
	if configure != nil {
		configure(ts, sw.ProjectID)
	}
	path := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("clone", gitTestOrigin))
	expectStatus(t, "create clone", r, b, http.StatusCreated)
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	for range 3 {
		ts.srv.managed.reconcileOnce(ctx)
	}
	a, err := ts.srv.store.LookupManagedActor(ctx, swarm, "clone")
	if err != nil || a.Phase != "running" {
		t.Fatalf("gateway actor: %+v %v", a, err)
	}
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("direct", "https://github.com/owner/public"))
	expectStatus(t, "create direct", r, b, http.StatusCreated)
	direct, err := ts.srv.store.LookupManagedActor(ctx, swarm, "direct")
	if err == nil {
		direct, err = ts.srv.store.ConfirmManagedActorStage(ctx, direct.UID, direct.Revision, direct.OperationID, "create", "stopped")
	}
	if err == nil {
		_, err = ts.srv.store.BeginManagedActorOperation(ctx, direct.UID, direct.Revision, store.NewID(), "start")
	}
	if err != nil {
		t.Fatalf("direct actor start intent: %v", err)
	}
	f := gitGatewayFixture{ts: ts, alice: alice, swarm: swarm, project: sw.ProjectID, actor: a, token: guestToken(ts.srv.managed.registry.relaySecret, a.UID, a.Incarnation)}
	f.upstream = newGitUpstream(t, ts, gitAdvertisement())
	return f
}

func (f gitGatewayFixture) base() string {
	return fmt.Sprintf("/api/proxmox/git/%s/%d", f.actor.UID, f.actor.Incarnation)
}

func (f gitGatewayFixture) serve(method, path string, body io.Reader, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	for key, values := range header {
		req.Header[key] = values
	}
	w := httptest.NewRecorder()
	f.ts.srv.Handler().ServeHTTP(w, req)
	return w
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

// FEATURE: Server-mediated Git gateway (#146).
// SPEC: Exactly one 43-character guest bearer token bound to the actor UID,
// generation, current start intent and swarm binding; operator cookies never
// authorize. The actor's current origin mode must be gateway on every request.
// IMPLEMENTATION: The same guest authorization as /api/proxmox/llm and secrets.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGitGatewayAuthorization(t *testing.T) {
	f := gitGatewaySetup(t)
	refs := f.base() + "/info/refs?service=git-upload-pack"
	direct, err := f.ts.srv.store.LookupManagedActor(context.Background(), f.swarm, "direct")
	if err != nil {
		t.Fatal(err)
	}
	directToken := guestToken(f.ts.srv.managed.registry.relaySecret, direct.UID, direct.Incarnation)
	next := f.actor.Incarnation + 1
	nextToken := guestToken(f.ts.srv.managed.registry.relaySecret, f.actor.UID, next)
	front, err := url.Parse(f.ts.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	cookie := http.Header{}
	for _, c := range f.alice.Jar.Cookies(front) {
		cookie.Add("Cookie", c.String())
	}
	if cookie.Get("Cookie") == "" {
		t.Fatal("operator session cookie missing")
	}
	withCookie := bearer("wrong")
	withCookie["Cookie"] = cookie["Cookie"]
	for _, tc := range []struct {
		name, path string
		header     http.Header
	}{
		{"no token", refs, nil},
		{"two tokens", refs, http.Header{"Authorization": {"Bearer " + f.token, "Bearer " + f.token}}},
		{"basic", refs, http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+f.token))}}},
		{"lowercase scheme", refs, http.Header{"Authorization": {"bearer " + f.token}}},
		{"wrong token", refs, bearer(strings.Repeat("A", 43))},
		{"truncated token", refs, bearer(f.token[:42])},
		{"other actor token", refs, bearer(directToken)},
		{"stale incarnation token", refs, bearer(nextToken)},
		{"future generation path", fmt.Sprintf("/api/proxmox/git/%s/%d/info/refs?service=git-upload-pack", f.actor.UID, next), bearer(nextToken)},
		{"other actor path", fmt.Sprintf("/api/proxmox/git/%s/%d/info/refs?service=git-upload-pack", direct.UID, direct.Incarnation), bearer(f.token)},
		{"cookie only", refs, cookie},
		{"cookie with wrong token", refs, withCookie},
	} {
		w := f.serve("GET", tc.path, nil, tc.header)
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), f.token) {
			t.Fatalf("%s: status=%d body=%s", tc.name, w.Code, w.Body.String())
		}
	}
	w := f.serve("GET", fmt.Sprintf("/api/proxmox/git/%s/%d/info/refs?service=git-upload-pack", direct.UID, direct.Incarnation), nil, bearer(directToken))
	if w.Code != http.StatusForbidden {
		t.Fatalf("direct-mode actor used the gateway: %d %s", w.Code, w.Body.String())
	}
	if seen := f.upstream.seen(); len(seen) != 0 {
		t.Fatalf("unauthorized requests reached upstream: %d", len(seen))
	}
	w = f.serve("GET", refs, nil, bearer(f.token))
	if w.Code != http.StatusOK || w.Body.String() != "advertisement" {
		t.Fatalf("authorized request: %d %s", w.Code, w.Body.String())
	}
	// Project of the actor now selects another configured remote: the actor
	// cannot reach it, and its own origin is no longer gateway.
	other := "https://git.example.com/group/other.git"
	f.ts.srv.git.remotes[other] = gitCredential{username: "brood-bot", token: gitTokenSentinel}
	setProjectRepository(t, f.ts, f.swarm, other)
	if w := f.serve("GET", refs, nil, bearer(f.token)); w.Code != http.StatusForbidden {
		t.Fatalf("other project repository: %d", w.Code)
	}
	setProjectRepository(t, f.ts, f.swarm, "")
	if w := f.serve("GET", refs, nil, bearer(f.token)); w.Code != http.StatusForbidden {
		t.Fatalf("cleared project repository: %d", w.Code)
	}
	setProjectRepository(t, f.ts, f.swarm, gitTestOrigin)
	delete(f.ts.srv.git.remotes, gitTestOrigin)
	if w := f.serve("GET", refs, nil, bearer(f.token)); w.Code != http.StatusForbidden {
		t.Fatalf("removed installation remote: %d", w.Code)
	}
	f.ts.srv.git.remotes[gitTestOrigin] = gitCredential{username: "brood-bot", token: gitTokenSentinel}
	seen := f.upstream.seen()
	if len(seen) != 1 || seen[0].path != "/group/repo.git/info/refs" || seen[0].query != "service=git-upload-pack" {
		t.Fatalf("upstream requests: %+v", seen)
	}
	current, err := f.ts.srv.store.GetManagedActor(context.Background(), f.actor.UID)
	if err == nil {
		_, err = f.ts.srv.store.BeginManagedActorOperation(context.Background(), current.UID, current.Revision, store.NewID(), "stop")
	}
	if err != nil {
		t.Fatal(err)
	}
	if w := f.serve("GET", refs, nil, bearer(f.token)); w.Code != http.StatusUnauthorized {
		t.Fatalf("stopped actor: %d", w.Code)
	}
}

// SPEC: Only the three smart-HTTP operations with exact methods, queries and
// content types are proxied; bodies are bounded before reaching upstream.
func TestGitGatewayRequestAllowlist(t *testing.T) {
	f := gitGatewaySetup(t)
	f.ts.srv.git.uploadLimit = 16
	base := f.base()
	auth := bearer(f.token)
	withType := func(contentType string) http.Header {
		h := bearer(f.token)
		if contentType != "" {
			h.Set("Content-Type", contentType)
		}
		return h
	}
	for _, tc := range []struct {
		method, path string
		body         io.Reader
		header       http.Header
		want         int
	}{
		{"GET", base + "/info/refs", nil, auth, 404},
		{"GET", base + "/info/refs?", nil, auth, 404},
		{"GET", base + "/info/refs?service=git-upload-pack&x=1", nil, auth, 404},
		{"GET", base + "/info/refs?service=git-upload-pack&service=git-upload-pack", nil, auth, 404},
		{"GET", base + "/info/refs?service=git%2Dupload-pack", nil, auth, 404},
		{"GET", base + "/info/refs?service=git-upload-archive", nil, auth, 404},
		{"GET", base + "/info/refs?service=git-upload-pack#x", nil, auth, 404},
		{"GET", base + "/info/refs/?service=git-upload-pack", nil, auth, 404},
		{"GET", base + "/HEAD", nil, auth, 404},
		{"GET", base + "/objects/info/packs", nil, auth, 404},
		{"GET", base + "/../info/refs?service=git-upload-pack", nil, auth, 404},
		{"GET", base + "/info%2Frefs?service=git-upload-pack", nil, auth, 404},
		{"GET", base, nil, auth, 404},
		{"GET", fmt.Sprintf("/api/proxmox/git/%s/0%d/info/refs?service=git-upload-pack", f.actor.UID, f.actor.Incarnation), nil, auth, 404},
		{"GET", fmt.Sprintf("/api/proxmox/git/%s/0/info/refs?service=git-upload-pack", f.actor.UID), nil, auth, 404},
		{"GET", "/api/proxmox/git/bad.uid/1/info/refs?service=git-upload-pack", nil, auth, 404},
		{"POST", base + "/info/refs?service=git-upload-pack", nil, auth, 405},
		{"GET", base + "/git-upload-pack", nil, auth, 405},
		{"PUT", base + "/git-receive-pack", nil, auth, 405},
		{"POST", base + "/git-upload-pack?x=1", nil, withType("application/x-git-upload-pack-request"), 404},
		{"POST", base + "/git-upload-pack", strings.NewReader("0000"), withType(""), 415},
		{"POST", base + "/git-upload-pack", strings.NewReader("0000"), withType("text/plain"), 415},
		{"POST", base + "/git-receive-pack", strings.NewReader("0000"), withType("application/x-git-upload-pack-request"), 415},
		{"POST", base + "/git-upload-pack", strings.NewReader("0000"), http.Header{"Authorization": {"Bearer " + f.token}, "Content-Type": {"application/x-git-upload-pack-request", "application/x-git-upload-pack-request"}}, 415},
		{"POST", base + "/git-upload-pack", strings.NewReader(strings.Repeat("0", 17)), withType("application/x-git-upload-pack-request"), 413},
		{"GET", base + "/info/refs?service=git-upload-pack", strings.NewReader("body"), auth, 400},
	} {
		w := f.serve(tc.method, tc.path, tc.body, tc.header)
		if w.Code != tc.want {
			t.Fatalf("%s %s: status=%d want=%d %s", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}
	if seen := f.upstream.seen(); len(seen) != 0 {
		t.Fatalf("refused requests reached upstream: %+v", seen)
	}
	// A streamed body without Content-Length is cut at the limit.
	req, err := http.NewRequest("POST", f.ts.http.URL+base+"/git-upload-pack", io.MultiReader(strings.NewReader(strings.Repeat("0", 1024))))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("streamed oversized body: %d", response.StatusCode)
	}
	if f.ts.srv.git.timeout != 30*time.Minute || newGitGateway(nil).uploadLimit != 64<<20 || newGitGateway(nil).receiveLimit != 1<<30 {
		t.Fatal("contract limits changed")
	}
}

// SPEC: Upstream receives only allowlisted headers plus Basic auth from the
// remote; the guest token, cookies and identity headers never leave the server.
// The client receives only Content-Type, Content-Encoding and Cache-Control.
func TestGitGatewayHeaders(t *testing.T) {
	f := gitGatewaySetup(t)
	f.upstream.setInner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Content-Encoding", "identity")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Location", "https://elsewhere.example/")
		_, _ = io.WriteString(w, "result")
	}))
	header := bearer(f.token)
	header.Set("Content-Type", "application/x-git-upload-pack-request")
	header.Set("Accept", "application/x-git-upload-pack-result")
	header.Set("Content-Encoding", "gzip")
	header.Set("Git-Protocol", "version=2")
	header.Set("Cookie", "session=operator-cookie-sentinel")
	header.Set("X-Forwarded-For", "10.0.0.1")
	header.Set("Forwarded", "for=10.0.0.1")
	header.Set("User-Agent", "actor-agent-sentinel")
	header.Set("X-Custom", "custom-sentinel")
	header.Set("Proxy-Authorization", "Basic proxy-sentinel")
	w := f.serve("POST", f.base()+"/git-upload-pack", strings.NewReader("0000"), header)
	if w.Code != http.StatusOK || w.Body.String() != "result" {
		t.Fatalf("proxied request: %d %s", w.Code, w.Body.String())
	}
	for key := range w.Header() {
		switch http.CanonicalHeaderKey(key) {
		case "Content-Type", "Content-Encoding", "Cache-Control", "Content-Length":
		default:
			t.Fatalf("response header %s leaked to the guest", key)
		}
	}
	if w.Header().Get("Content-Encoding") != "identity" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("allowlisted response headers dropped: %v", w.Header())
	}
	seen := f.upstream.seen()
	if len(seen) != 1 {
		t.Fatalf("upstream requests: %d", len(seen))
	}
	got := seen[0].header
	if got.Get("Content-Type") != "application/x-git-upload-pack-request" || got.Get("Accept") != "application/x-git-upload-pack-result" || got.Get("Content-Encoding") != "gzip" || got.Get("Git-Protocol") != "version=2" {
		t.Fatalf("allowlisted request headers dropped: %v", got)
	}
	if user, password, ok := (&http.Request{Header: got}).BasicAuth(); !ok || user != "brood-bot" || password != gitTokenSentinel || len(got.Values("Authorization")) != 1 {
		t.Fatal("upstream did not receive exactly the installation credential")
	}
	for key, values := range got {
		joined := strings.Join(values, " ")
		if strings.Contains(joined, f.token) || strings.Contains(joined, "sentinel") && key != "Authorization" {
			t.Fatalf("guest header %s reached upstream", key)
		}
		switch http.CanonicalHeaderKey(key) {
		case "Cookie", "X-Forwarded-For", "Forwarded", "X-Custom", "Proxy-Authorization", "X-Forwarded-Host", "X-Forwarded-Proto":
			t.Fatalf("header %s reached upstream", key)
		}
	}
}

// SPEC: Redirects are refused rather than followed; upstream failures are
// generic and never echo credentials or upstream bodies.
func TestGitGatewayRefusesRedirectsAndHidesUpstreamErrors(t *testing.T) {
	f := gitGatewaySetup(t)
	refs := f.base() + "/info/refs?service=git-upload-pack"
	f.upstream.setInner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/repo.git/info/refs?service=git-upload-pack", http.StatusFound)
	}))
	w := f.serve("GET", refs, nil, bearer(f.token))
	if w.Code != http.StatusBadGateway || w.Header().Get("Location") != "" || len(f.upstream.seen()) != 1 {
		t.Fatalf("redirect: %d %v", w.Code, w.Header())
	}
	f.ts.srv.git.remotes[gitTestOrigin] = gitCredential{username: "brood-bot", token: "wrong-" + gitTokenSentinel}
	w = f.serve("GET", refs, nil, bearer(f.token))
	body := w.Body.String()
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "" || w.Header().Get("Set-Cookie") != "" || strings.Contains(body, gitTokenSentinel) || strings.Contains(body, "brood-bot") {
		t.Fatalf("upstream refusal: %d %v %s", w.Code, w.Header(), body)
	}
	f.ts.srv.git.transport.CloseIdleConnections()
	f.ts.srv.git.transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial failed for brood-bot:" + gitTokenSentinel)
	}
	w = f.serve("GET", refs, nil, bearer(f.token))
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), gitTokenSentinel) || strings.Contains(w.Body.String(), "git.example.com") {
		t.Fatalf("transport failure: %d %s", w.Code, w.Body.String())
	}
}

// SPEC: At most a fixed number of concurrent requests per actor; the overall
// deadline bounds slow upstreams; revocation cancels an open stream.
func TestGitGatewayConcurrencyTimeoutAndRevocation(t *testing.T) {
	f := gitGatewaySetup(t)
	refs := f.ts.http.URL + f.base() + "/info/refs?service=git-upload-pack"
	release := make(chan struct{})
	entered := make(chan struct{}, 16)
	f.upstream.setInner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, "advertisement")
	}))
	get := func() (int, error) {
		req, _ := http.NewRequest("GET", refs, nil)
		req.Header.Set("Authorization", "Bearer "+f.token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode, nil
	}
	limit := f.ts.srv.git.perActor
	if limit < 1 || limit > 8 {
		t.Fatalf("per-actor limit: %d", limit)
	}
	results := make(chan int, limit)
	for range limit {
		go func() {
			code, _ := get()
			results <- code
		}()
	}
	for range limit {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent requests did not reach upstream")
		}
	}
	if code, err := get(); err != nil || code != http.StatusTooManyRequests {
		t.Fatalf("request over the per-actor limit: %d %v", code, err)
	}
	close(release)
	for range limit {
		if code := <-results; code != http.StatusOK {
			t.Fatalf("admitted request: %d", code)
		}
	}
	if code, err := get(); err != nil || code != http.StatusOK {
		t.Fatalf("slot not released: %d %v", code, err)
	}

	f.ts.srv.git.timeout = 200 * time.Millisecond
	blocked := make(chan struct{})
	defer close(blocked)
	f.upstream.setInner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-blocked:
		case <-r.Context().Done():
		}
	}))
	started := time.Now()
	if code, err := get(); err != nil || code != http.StatusGatewayTimeout || time.Since(started) > 5*time.Second {
		t.Fatalf("deadline: %d %v after %v", code, err, time.Since(started))
	}

	f.ts.srv.git.timeout = time.Minute
	canceled := make(chan struct{})
	f.upstream.setInner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-blocked:
		}
	}))
	req, _ := http.NewRequest("GET", refs, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, 5)
	if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "first" {
		t.Fatalf("stream did not start: %q %v", first, err)
	}
	setProjectRepository(t, f.ts, f.swarm, "")
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("open stream survived revocation of the gateway mode")
	}
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Log("stream ended without a transport error")
	}
}
