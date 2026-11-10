//go:build linux

package guest

// FEATURE: Fixed-loopback Git gateway of the root supervisor (#147).
// SPEC: Only Host 127.0.0.1:8318 and the three smart-HTTP paths of
// /origin.git reach the server; actor Authorization and cookies are dropped,
// the scoped relay token is injected; bodies stream both ways without the
// model gateway's 8 MiB buffer; redirects are refused; concurrency is bounded.
// IMPLEMENTATION: httputil.ReverseProxy with an allowlisted request/response
// header set against an httptest TLS server standing in for Brood.

import (
	"bytes"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

var gitTestToken = strings.Repeat("t", 42) + "1"

func gitTestConfig(backend *httptest.Server) relay.ClientConfig {
	return relay.ClientConfig{URL: "wss" + strings.TrimPrefix(backend.URL, "https") + "/api/proxmox/guest/actor/1", ActorUID: "actor", Generation: 1, Token: gitTestToken, CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})}
}

func gitTestRequest(method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Host = gitAddress
	return request
}

func TestGitGatewayRoutesOnlySmartHTTPAndIsolatesCredentials(t *testing.T) {
	var calls atomic.Int32
	seen := make(chan *http.Request, 8)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		seen <- r
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Set-Cookie", "forbidden=value")
		w.Header().Set("X-Upstream-Secret", "forbidden")
		io.WriteString(w, "advertisement")
	}))
	defer backend.Close()
	handler, err := NewGitGateway(gitTestConfig(backend))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, target, upstream, query string }{
		{"GET", "/origin.git/info/refs?service=git-upload-pack", "/api/proxmox/git/actor/1/info/refs", "service=git-upload-pack"},
		{"GET", "/origin.git/info/refs?service=git-receive-pack", "/api/proxmox/git/actor/1/info/refs", "service=git-receive-pack"},
		{"POST", "/origin.git/git-upload-pack", "/api/proxmox/git/actor/1/git-upload-pack", ""},
		{"POST", "/origin.git/git-receive-pack", "/api/proxmox/git/actor/1/git-receive-pack", ""},
	} {
		var body io.Reader
		if tc.method == "POST" {
			body = strings.NewReader("0000")
		}
		request := gitTestRequest(tc.method, tc.target, body)
		request.Header.Set("Authorization", "Basic actor-supplied")
		request.Header.Set("Cookie", "session=forbidden")
		request.Header.Set("X-Forwarded-For", "203.0.113.9")
		request.Header.Set("Git-Protocol", "version=2")
		request.Header.Set("Accept", "application/x-git-upload-pack-result")
		if tc.method == "POST" {
			request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
			request.Header.Set("Content-Encoding", "gzip")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 || response.Body.String() != "advertisement" {
			t.Fatalf("%s %s: %d %q", tc.method, tc.target, response.Code, response.Body)
		}
		if response.Header().Get("Set-Cookie") != "" || response.Header().Get("X-Upstream-Secret") != "" || response.Header().Get("Cache-Control") != "no-cache" || response.Header().Get("Content-Type") != "application/x-git-upload-pack-advertisement" {
			t.Fatalf("response headers: %v", response.Header())
		}
		r := <-seen
		if r.Method != tc.method || r.URL.Path != tc.upstream || r.URL.RawQuery != tc.query {
			t.Fatalf("upstream route %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer "+gitTestToken || len(r.Header.Values("Authorization")) != 1 || r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Git-Protocol") != "version=2" || r.Header.Get("Accept") != "application/x-git-upload-pack-result" {
			t.Fatalf("upstream headers: %v", r.Header)
		}
		if tc.method == "POST" && (r.Header.Get("Content-Type") != "application/x-git-upload-pack-request" || r.Header.Get("Content-Encoding") != "gzip") {
			t.Fatalf("upstream body headers: %v", r.Header)
		}
	}
	denied := []struct {
		method, target, host string
		header               [2]string
		body                 string
		length               int64
	}{
		{method: "GET", target: "/origin.git/info/refs?service=git-upload-pack", host: "localhost:8318"},
		{method: "GET", target: "/origin.git/info/refs?service=git-upload-pack", host: "127.0.0.1:8317"},
		{method: "GET", target: "/origin.git/info/refs"},
		{method: "GET", target: "/origin.git/info/refs?service=git-upload-pack&x=1"},
		{method: "GET", target: "/origin.git/info/refs?service=git-upload-archive"},
		{method: "GET", target: "/origin.git/info/refs?service=git-upload-pack#x"},
		{method: "GET", target: "/origin.git/HEAD"},
		{method: "GET", target: "/origin.git/objects/info/packs"},
		{method: "GET", target: "/other.git/info/refs?service=git-upload-pack"},
		{method: "GET", target: "/origin.git//info/refs?service=git-upload-pack"},
		{method: "GET", target: "/origin.git/%69nfo/refs?service=git-upload-pack"},
		{method: "POST", target: "/origin.git/info/refs?service=git-upload-pack"},
		{method: "GET", target: "/origin.git/git-upload-pack"},
		{method: "PUT", target: "/origin.git/git-receive-pack"},
		{method: "POST", target: "/origin.git/git-upload-pack?service=git-upload-pack"},
		{method: "GET", target: "/origin.git/info/refs?service=git-upload-pack", header: [2]string{"Origin", "http://127.0.0.1:8318"}},
		{method: "GET", target: "/origin.git/info/refs?service=git-upload-pack", body: "unexpected"},
		{method: "POST", target: "/origin.git/git-upload-pack", length: gitUploadLimit + 1},
		{method: "POST", target: "/origin.git/git-receive-pack", length: gitReceiveLimit + 1},
	}
	for _, tc := range denied {
		request := gitTestRequest(tc.method, tc.target, strings.NewReader(tc.body))
		if tc.host != "" {
			request.Host = tc.host
		}
		if tc.header[0] != "" {
			request.Header.Set(tc.header[0], tc.header[1])
		}
		if tc.length != 0 {
			request.ContentLength = tc.length
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 {
			t.Errorf("%s %s host=%q accepted: %d", tc.method, tc.target, tc.host, response.Code)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("denied requests reached the server: %d calls", calls.Load())
	}
}

func TestGitGatewayRefusesRedirectsAndCredentialPrompts(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("service") == "git-receive-pack" {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "https://elsewhere.example/repo.git/info/refs", http.StatusFound)
	}))
	defer backend.Close()
	handler, err := NewGitGateway(gitTestConfig(backend))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gitTestRequest("GET", "/origin.git/info/refs?service=git-upload-pack", nil))
	if response.Code != http.StatusBadGateway || response.Header().Get("Location") != "" {
		t.Fatalf("redirect followed or forwarded: %d %v", response.Code, response.Header())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, gitTestRequest("GET", "/origin.git/info/refs?service=git-receive-pack", nil))
	if response.Code != http.StatusForbidden || response.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("credential prompt forwarded to actor Git: %d %v", response.Code, response.Header())
	}
}

func TestGitGatewayStreamsLargeBodiesBothWays(t *testing.T) {
	const chunk = 1 << 20
	const total = 10 * chunk // above the model gateway's 8 MiB buffer
	firstUpload, firstDownload := make(chan struct{}), make(chan struct{})
	received := make(chan int, 1)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			if _, err := io.ReadFull(r.Body, make([]byte, chunk)); err != nil {
				t.Error(err)
				return
			}
			close(firstUpload)
			rest, _ := io.Copy(io.Discard, r.Body)
			received <- chunk + int(rest)
			io.WriteString(w, "ok")
			return
		}
		io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		select {
		case <-firstDownload:
		case <-time.After(5 * time.Second):
			t.Error("response was buffered by the gateway")
		}
		io.WriteString(w, "second")
	}))
	defer backend.Close()
	var handler http.Handler
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	defer front.Close()
	var err error
	if handler, err = newGitGateway(gitTestConfig(backend), front.Listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	go func() {
		writer.Write(bytes.Repeat([]byte("a"), chunk))
		select {
		case <-firstUpload:
		case <-time.After(5 * time.Second):
			writer.CloseWithError(io.ErrUnexpectedEOF)
			return
		}
		for i := 1; i < total/chunk; i++ {
			writer.Write(bytes.Repeat([]byte("b"), chunk))
		}
		writer.Close()
	}()
	response, err := http.Post(front.URL+"/origin.git/git-receive-pack", "application/x-git-receive-pack-request", reader)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || <-received != total {
		t.Fatalf("request body was not streamed intact: %d", response.StatusCode)
	}
	response, err = http.Post(front.URL+"/origin.git/git-upload-pack", "application/x-git-upload-pack-request", strings.NewReader("0000"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len("first"))
	if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "first" {
		t.Fatalf("first response bytes: %q %v", first, err)
	}
	close(firstDownload)
	rest, _ := io.ReadAll(response.Body)
	if string(rest) != "second" {
		t.Fatalf("response tail: %q", rest)
	}
}

func TestGitGatewayBoundsConcurrency(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, gitConcurrency)
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	defer backend.Close()
	handler, err := NewGitGateway(gitTestConfig(backend))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, gitConcurrency)
	for i := 0; i < gitConcurrency; i++ {
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, gitTestRequest("GET", "/origin.git/info/refs?service=git-upload-pack", nil))
			done <- response.Code
		}()
		<-entered
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, gitTestRequest("GET", "/origin.git/info/refs?service=git-upload-pack", nil))
	close(release)
	for i := 0; i < gitConcurrency; i++ {
		<-done
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("request above the concurrency bound: %d", response.Code)
	}
}

func TestGitGatewayRequiresSupervisorIdentity(t *testing.T) {
	for _, cfg := range []relay.ClientConfig{
		{URL: "wss://brood.example", ActorUID: "actor", Generation: 1, Token: "short"},
		{URL: "https://brood.example", ActorUID: "actor", Generation: 1, Token: gitTestToken},
		{URL: "wss://brood.example/api/proxmox/guest/other/1", ActorUID: "actor", Generation: 1, Token: gitTestToken},
		{URL: "wss://brood.example", ActorUID: "../actor", Generation: 1, Token: gitTestToken},
	} {
		if _, err := NewGitGateway(cfg); err == nil {
			t.Errorf("invalid supervisor identity accepted: %+v", cfg.URL)
		}
	}
	if !strings.Contains(nftRules(), `oifname "lo" ip daddr 127.0.0.0/8 meta l4proto { tcp, udp } accept`) || !strings.Contains(nftRules(), `iifname != "lo" tcp dport { 3128, 5353, 8080, 8317, 8318 } reject`) {
		t.Fatal("Git gateway missing from fixed loopback policy")
	}
}
