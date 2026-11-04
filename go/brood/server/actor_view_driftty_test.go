package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// FEATURE: Deliver driftty to existing managed actors without replacing their CT.
// SPEC: Replace only successful root HTML behind actor-view authority. Keep the
// opaque-origin sandbox and leave token, WebSocket, native and error responses intact.
type observedViewBody struct {
	io.Reader
	closed bool
}

func (b *observedViewBody) Close() error { b.closed = true; return nil }

func TestEmbeddedDrifttyProvenance(t *testing.T) {
	// FEATURE: #163: the embedded actor-view uses the same direct PTY client as the guest.
	// SPEC: Keep MIT attribution and opaque-origin storage; no multiplexer actions.
	if strings.Contains(strings.ToLower(string(drifttyClient)), "tmux") {
		t.Fatal("embedded client still exposes multiplexer controls")
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(drifttyClient)); got != "aa7df400abe4d53aef4dbf3cf812273d3e4fd69a69d09cd141f93b166e2e384d" {
		t.Fatalf("unexpected terminal client checksum: %s", got)
	}
	for _, required := range []string{"MIT License", "Copyright (c) 2026 Mark Percival", "memoryStorage"} {
		if !strings.Contains(string(drifttyClient), required) {
			t.Fatalf("missing client provenance or sandbox adapter: %s", required)
		}
	}
}

func TestManagedActorViewUsesDriftty(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://brood.test/swarms/swarm/actors/master/", nil)
	req = req.WithContext(context.WithValue(req.Context(), actorViewKey{}, actorView{managedUID: "owned", swarm: "swarm", actor: "master", origin: "https://brood.test", token: "ticket"}))
	original := &observedViewBody{Reader: strings.NewReader("stock ttyd")}
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Content-Encoding": {"gzip"}, "ETag": {"old"}}, Body: original, ContentLength: 10}
	if err := (&Server{}).actorViewResponse(req, response); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<title>driftty — Terminal</title>") {
		t.Fatal("managed root still serves the guest's stock client")
	}
	if !original.closed {
		t.Fatal("replaced upstream body leaked")
	}
	if response.ContentLength != int64(len(body)) || response.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatal("replacement length not updated")
	}
	if response.Header.Get("Content-Encoding") != "" || response.Header.Get("ETag") != "" {
		t.Fatal("upstream encoding or validator survived replacement")
	}
	if !strings.Contains(response.Header.Get("Content-Security-Policy"), "sandbox allow-scripts;") || strings.Contains(response.Header.Get("Content-Security-Policy"), "allow-same-origin") {
		t.Fatal("terminal sandbox changed")
	}
	if response.Header.Get("Access-Control-Allow-Origin") != "null" {
		t.Fatal("opaque-origin transport lost")
	}
}

func TestDrifttyReplacementIsScopedToManagedRootHTML(t *testing.T) {
	for _, tc := range []struct {
		name, path, uid, contentType, method string
		status                               int
		view                                 bool
	}{
		{"token", "/swarms/swarm/actors/master/token", "owned", "application/json", "GET", 200, true},
		{"socket", "/swarms/swarm/actors/master/ws", "owned", "text/html", "GET", 101, true},
		{"native", "/swarms/swarm/actors/master/", "", "text/html", "GET", 200, true},
		{"outside view", "/swarms/swarm/actors/master/", "owned", "text/html", "GET", 200, false},
		{"upstream error", "/swarms/swarm/actors/master/", "owned", "text/html", "GET", 503, true},
		{"not html", "/swarms/swarm/actors/master/", "owned", "application/octet-stream", "GET", 200, true},
		{"head", "/swarms/swarm/actors/master/", "owned", "text/html", "HEAD", 200, true},
		{"other path", "/swarms/swarm/actors/master/other", "owned", "text/html", "GET", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "https://brood.test"+tc.path, nil)
			if tc.view {
				req = req.WithContext(context.WithValue(req.Context(), actorViewKey{}, actorView{managedUID: tc.uid, swarm: "swarm", actor: "master", origin: "https://brood.test", token: "ticket"}))
			}
			body := &observedViewBody{Reader: strings.NewReader("original")}
			response := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.contentType}}, Body: body}
			if err := (&Server{}).actorViewResponse(req, response); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "original" || body.closed {
				t.Fatal("unrelated upstream response was replaced")
			}
		})
	}
}
