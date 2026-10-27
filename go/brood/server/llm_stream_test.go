package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLLMStreamFlush(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: last\n\n")
	}))
	defer backend.Close()
	defer close(release)
	ts := newLLMTestServer(t, backend.URL)
	// A buffered proxy deadlocks until this timeout: the backend cannot finish
	// until the client has received the first event and released it.
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(ts.http.URL + "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "data: first\n\n" {
		t.Fatalf("stream buffered: %q %v", first, err)
	}
}

func TestLLMWebSocket(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" || r.Header.Get("Cookie") != "" {
			t.Errorf("upgrade headers: %v", r.Header)
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		// ReverseProxy tunnels bytes after the handshake, including RFC6455 frames.
		frame := make([]byte, 4)
		if _, err := io.ReadFull(rw, frame); err != nil {
			t.Error(err)
			return
		}
		if string(frame) != "ping" {
			t.Errorf("upgrade payload: %q", frame)
		}
		rw.WriteString("pong")
		rw.Flush()
	}))
	defer backend.Close()
	ts := newLLMTestServer(t, backend.URL)
	req, _ := http.NewRequest("GET", ts.http.URL+"/v1/realtime", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Authorization", "Bearer native-key")
	req.Header.Set("Cookie", "brood_session=invalid")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 101 {
		t.Fatalf("upgrade status: %d", resp.StatusCode)
	}
	stream, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatal("upgrade has no writable stream")
	}
	stream.Write([]byte("ping"))
	frame := make([]byte, 4)
	if _, err := io.ReadFull(stream, frame); err != nil || string(frame) != "pong" {
		t.Fatalf("tunnel reply: %q %v", frame, err)
	}
}

func TestLLMManagementUpgradeCSRF(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "root-password-1")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	for _, p := range []string{"/v8/management/events", "/v0/management/plugin/events"} {
		req, _ := http.NewRequest("GET", ts.http.URL+p, nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Origin", ts.http.URL)
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("upgrade without CSRF: %d", resp.StatusCode)
		}
	}
}

func TestLLMPublicPathBoundaries(t *testing.T) {
	for _, p := range []string{"/v1/../v8/management/config", "/v1//models", "/v1", "/v8/management/config", "/v0/resource/plugins/a", "/callback/extra", "/anthropic/callback/extra", "/other"} {
		if publicLLMPath(p) {
			t.Errorf("unexpected native-client bypass for %s", p)
		}
	}
	for _, p := range []string{"/v1/", "/v1/models", "/callback"} {
		if !publicLLMPath(p) {
			t.Errorf("missing native route: %s", p)
		}
	}
}

func TestLLMEncodedTraversal(t *testing.T) {
	calls := make(chan string, 8)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- r.URL.Path
		io.WriteString(w, `{}`)
	}))
	defer backend.Close()
	ts := newLLMTestServer(t, backend.URL)
	for _, p := range []string{"/v1/%2e%2e/v8/management/config", "/v1%2f..%2fv8/management/config"} {
		resp, _, _ := ts.do(newClient(t), "GET", p, "", nil)
		if resp.StatusCode != 404 {
			t.Errorf("encoded traversal %s: %d", p, resp.StatusCode)
		}
	}
	select {
	case p := <-calls:
		t.Fatalf("noncanonical public path reached backend: %s", p)
	default:
	}
}

func TestLLMPublicWithoutManagementKey(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) }))
	defer backend.Close()
	cfg := Config{DBPath: t.TempDir() + "/brood.db", CLIProxyURL: backend.URL}
	srv, err := New(nil, cfg, Options{Provisioner: newFakeProvisioner()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := &testServer{t: t, srv: srv, http: httptest.NewServer(srv.Handler())}
	defer ts.http.Close()
	resp, data, _ := ts.do(newClient(t), "POST", "/v1/responses", "", nil)
	expectStatus(t, "native without management key", resp, data, 200)
	ts.bootstrapAdmin("root", "root-password-1")
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	resp, data, raw := ts.do(admin, "GET", "/v8/management/config", "", nil)
	expectStatus(t, "management requires key", resp, data, 503)
	if !strings.Contains(raw, "CLIPROXY_MANAGEMENT_KEY") {
		t.Fatalf("missing key refusal: %s", raw)
	}
}
