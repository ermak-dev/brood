package proxy

// FEATURE: Injected provider transport for HTTP and upgrades.
// SPEC: Both paths dial only the server-resolved target through the injected
// dialer; native HTTPS retains RouterTLS, and authorization still precedes I/O.

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/app"
)

func TestManagedTransportHTTPAndUpgrade(t *testing.T) {
	var calls atomic.Int32
	cfg := Config{Swarms: fakeSwarms{"s": {ID: "s", AtespaceName: "ns", State: app.StateActive}}, Authorize: func(string, app.Action, app.Resource) (bool, error) { return true, nil }, CurrentUser: fixedUser("u", "user"), Upstream: &fakeResolver{bases: map[string]*url.URL{"ns/a": {Scheme: "http", Host: "managed.invalid:8080"}}}}
	cfg.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		calls.Add(1)
		if network != "tcp" || address != "managed.invalid:8080" {
			t.Errorf("unexpected dial %s %s", network, address)
		}
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			r, err := http.ReadRequest(bufio.NewReader(b))
			if err != nil {
				return
			}
			if r.Header.Get("Upgrade") != "" {
				io.WriteString(b, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nhello")
				return
			}
			io.WriteString(b, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")
		}()
		return a, nil
	}
	p := New(cfg)
	defer p.Close()
	mux := http.NewServeMux()
	p.Mount(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/swarms/s/actors/a/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello" {
		t.Fatalf("HTTP failed: %d %q", resp.StatusCode, body)
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /swarms/s/actors/a/ HTTP/1.1\r\nHost: panel.invalid\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 101 {
		t.Fatalf("upgrade failed: %d", response.StatusCode)
	}
	b := make([]byte, 5)
	if _, err := io.ReadFull(reader, b); err != nil || string(b) != "hello" {
		t.Fatalf("upgrade data: %q %v", b, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("provider dial calls = %d, want 2", calls.Load())
	}
}
