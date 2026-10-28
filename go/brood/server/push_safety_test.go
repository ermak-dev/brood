package server

// FEATURE: Push rejects malformed scope, descriptors, and unbounded upload input.
// SPEC: 32 MiB chunks, 8 GiB blobs, 30-minute session-bound uploads and no registry redirects.
// IMPLEMENTATION: Origin validation, bounded upload state and streaming body limits.
// RESULT: IMPLEMENTED — limits, expiry, revocation and scope rejection verified.
// STATUS: DONE

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/auth"
)

func TestPushRegistryConfigValidation(t *testing.T) {
	for _, host := range []string{"localhost:65536", "localhost:0", "bad..host:5000", "-bad:5000", "https://host:5000", "host:abc"} {
		_, err := LoadConfig(func(k string) string {
			switch k {
			case "BROOD_REGISTRY_URL":
				return "http://registry:5000"
			case "BROOD_REGISTRY_IMAGE_HOST":
				return host
			}
			return ""
		})
		if err == nil {
			t.Errorf("accepted unsafe image host %q", host)
		}
	}
	for _, origin := range []string{"http://registry/", "http://registry?x=y", "http://registry#fragment", "ftp://registry", "http://registry:65536"} {
		_, err := LoadConfig(func(k string) string {
			switch k {
			case "BROOD_REGISTRY_URL":
				return origin
			case "BROOD_REGISTRY_IMAGE_HOST":
				return "localhost:5002"
			}
			return ""
		})
		if err == nil {
			t.Errorf("accepted unsafe registry origin %q", origin)
		}
	}
	cfg, err := LoadConfig(func(k string) string {
		switch k {
		case "BROOD_REGISTRY_URL":
			return "http://registry:5000"
		case "BROOD_REGISTRY_IMAGE_HOST":
			return "localhost:5002"
		}
		return ""
	})
	if err != nil || cfg.RegistryURL != "http://registry:5000" || cfg.RegistryImageHost != "localhost:5002" {
		t.Fatalf("paired config: %+v, %v", cfg, err)
	}
}

func TestPushUploadSafety(t *testing.T) {
	var calls atomic.Int64
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.Method {
		case "POST":
			w.Header().Set("Location", r.URL.Path+"registry-id?_state=private")
			w.WriteHeader(202)
		case "PATCH":
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Location", r.URL.Path+"?_state=next")
			w.WriteHeader(202)
		default:
			w.WriteHeader(201)
		}
	}))
	defer registry.Close()
	ts := newSubstrateServer(t, newFakeAteapi(), func(c *Config) { c.RegistryURL = registry.URL; c.RegistryImageHost = "localhost:5002" })
	admin, alice, _, id := actorsSetup(t, ts.testServer)
	base := "/api/swarms/" + id + "/builds/dev"
	start := func() string {
		resp, data, _ := ts.do(alice, "POST", base+"/uploads", ts.csrf(alice), nil)
		expectStatus(t, "start", resp, data, 201)
		return data["upload_id"].(string)
	}
	upload := start()
	patch := func(length int64, media string, body io.ReadCloser) int {
		req := httptest.NewRequest("PATCH", ts.http.URL+base+"/uploads/"+upload, body)
		req.ContentLength = length
		req.Header.Set("Content-Type", media)
		req.Header.Set("Origin", ts.http.URL)
		req.Header.Set(auth.HeaderCSRF, ts.csrf(alice))
		for _, cookie := range alice.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		ts.srv.Handler().ServeHTTP(w, req)
		return w.Code
	}
	for _, tc := range []struct {
		length int64
		media  string
		want   int
	}{{-1, "application/octet-stream", 411}, {pushChunkLimit + 1, "application/octet-stream", 413}, {1, "application/json", 415}} {
		before := calls.Load()
		if got := patch(tc.length, tc.media, io.NopCloser(strings.NewReader("x"))); got != tc.want {
			t.Errorf("chunk length %d type %s: %d", tc.length, tc.media, got)
		}
		if calls.Load() != before {
			t.Fatal("invalid chunk reached registry")
		}
	}
	if got := patch(1<<20, "application/octet-stream", io.NopCloser(bytes.NewReader(make([]byte, 1<<20)))); got != 204 {
		t.Fatalf("stream larger than JSON helpers: %d", got)
	}
	ts.srv.push.mu.Lock()
	state := ts.srv.push.uploads[upload]
	ts.srv.push.mu.Unlock()
	state.mu.Lock()
	state.size = pushBlobLimit
	state.mu.Unlock()
	if got := patch(1, "application/octet-stream", io.NopCloser(strings.NewReader("x"))); got != 413 {
		t.Fatalf("blob limit: %d", got)
	}
	state.mu.Lock()
	state.expires = time.Now().Add(-time.Second)
	state.mu.Unlock()
	if got := patch(1, "application/octet-stream", io.NopCloser(strings.NewReader("x"))); got != 404 {
		t.Fatalf("expired upload: %d", got)
	}
	upload = start()
	resp, data, _ := ts.do(admin, "DELETE", "/api/projects/"+func() string { sw, _, _ := ts.srv.gateway.SwarmByID(id); return sw.ProjectID }()+"/members/alice", ts.csrf(admin), nil)
	expectStatus(t, "revoke membership", resp, data, 204)
	before := calls.Load()
	if got := patch(1, "application/octet-stream", io.NopCloser(strings.NewReader("x"))); got != 404 {
		t.Fatalf("revoked membership: %d", got)
	}
	if calls.Load() != before {
		t.Fatal("revoked request reached registry")
	}
}

func TestPushLocationScope(t *testing.T) {
	p := &pushRegistry{}
	p.origin, _ = url.Parse("http://registry:5000")
	base := "http://registry:5000/v2/swarm-123/dev/blobs/uploads/"
	for _, location := range []string{"https://registry:5000/v2/swarm-123/dev/blobs/uploads/id", "http://evil/v2/swarm-123/dev/blobs/uploads/id", "/v2/swarm-other/dev/blobs/uploads/id", "/v2/swarm-123/dev/blobs/uploads/../id", "/v2/swarm-123/dev/blobs/uploads/id?mount=sha256:abc", "/v2/swarm-123/dev/blobs/uploads/id?from=other", "/v2/swarm-123/dev/blobs/uploads/id#fragment", "/v2/swarm-123/dev/blobs/uploads/%2fother"} {
		if _, err := p.uploadLocation(base, location, "swarm-123/dev"); err == nil {
			t.Errorf("accepted location %q", location)
		}
	}
	if _, err := p.uploadLocation(base, "id?_state=secret", "swarm-123/dev"); err != nil {
		t.Fatal(err)
	}
}

func TestPushDescriptorValidation(t *testing.T) {
	size := int64(1)
	valid := pushDescriptor{MediaType: pushConfigType, Digest: pushDigest("config"), Size: &size}
	if !validPushDescriptor(valid, "config") {
		t.Fatal("valid config rejected")
	}
	for _, change := range []func(*pushDescriptor){func(d *pushDescriptor) { d.URLs = []string{"https://evil/layer"} }, func(d *pushDescriptor) { v := int64(-1); d.Size = &v }, func(d *pushDescriptor) { d.Size = nil }, func(d *pushDescriptor) { d.Digest = "sha256:bad" }, func(d *pushDescriptor) { d.MediaType = "application/x-unknown" }, func(d *pushDescriptor) { d.Data = json.RawMessage(`"inline"`) }} {
		candidate := valid
		change(&candidate)
		if validPushDescriptor(candidate, "config") {
			t.Errorf("invalid descriptor accepted: %+v", candidate)
		}
	}
}
