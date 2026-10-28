package server

// FEATURE: OCI manifests are verified before registry publication.
// SPEC: Scoped HEAD checks, indexes, bounded JSON, sanitized errors and no redirects.
// IMPLEMENTATION: Digest and descriptor validation before scoped registry writes.
// RESULT: IMPLEMENTED — valid index publication and hostile manifest rejection verified.
// STATUS: DONE

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPushManifestSafety(t *testing.T) {
	var status atomic.Int64
	status.Store(200)
	var puts atomic.Int64
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "HEAD":
			w.Header().Set("Content-Length", "6")
			if strings.Contains(r.URL.Path, "/manifests/") {
				w.Header().Set("Content-Length", "123")
				w.Header().Set("Content-Type", pushManifestType)
			}
			w.WriteHeader(int(status.Load()))
		case "PUT":
			puts.Add(1)
			w.WriteHeader(201)
		case "POST":
			w.Header().Set("Location", "http://should-not-follow.invalid/private")
			w.WriteHeader(307)
			_, _ = w.Write([]byte("secret registry response"))
		}
	}))
	defer registry.Close()
	ts := newSubstrateServer(t, newFakeAteapi(), func(c *Config) { c.RegistryURL = registry.URL; c.RegistryImageHost = "localhost:5002" })
	_, alice, _, id := actorsSetup(t, ts.testServer)
	base := "/api/swarms/" + id + "/builds/dev"
	manifest := `{"schemaVersion":2,"mediaType":"` + pushManifestType + `","config":{"mediaType":"` + pushConfigType + `","digest":"` + pushDigest("config") + `","size":6},"layers":[]}`
	put := func(body string, want int) {
		req, _ := http.NewRequest("PUT", ts.http.URL+base+"/manifests/"+pushDigest(body), strings.NewReader(body))
		req.Header.Set("Origin", ts.http.URL)
		req.Header.Set("X-CSRFToken", ts.csrf(alice))
		resp, err := alice.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var data map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&data)
		expectStatus(t, "manifest safety", resp, data, want)
	}
	put(strings.Replace(manifest, `"schemaVersion":2`, `"schemaVersion":1`, 1), 400)
	put(strings.Replace(manifest, `"size":6`, `"size":-1`, 1), 400)
	put(strings.Replace(manifest, `"size":6`, `"size":6,"urls":["https://foreign.invalid/layer"]`, 1), 400)
	status.Store(404)
	put(manifest, 400)
	status.Store(500)
	put(manifest, 502)
	if puts.Load() != 0 {
		t.Fatal("invalid manifest published")
	}
	status.Store(200)
	index := `{"schemaVersion":2,"mediaType":"` + pushIndexType + `","manifests":[{"mediaType":"` + pushManifestType + `","digest":"` + pushDigest(manifest) + `","size":123}]}`
	put(strings.Replace(index, `"size":123`, `"size":0`, 1), 400)
	put(strings.Replace(index, `"mediaType":"`+pushManifestType+`"`, `"mediaType":"`+pushIndexType+`"`, 1), 400)
	put(index, 200)
	if puts.Load() != 1 {
		t.Fatal("index not published")
	}
	put(strings.Repeat(" ", int(maxResourceBody)+1), 413)
	resp, data, raw := ts.do(alice, "POST", base+"/uploads", ts.csrf(alice), nil)
	expectStatus(t, "redirect rejected", resp, data, 502)
	if strings.Contains(raw, "secret") || strings.Contains(raw, "private") || strings.Contains(raw, "invalid") {
		t.Fatalf("registry error leaked %q", raw)
	}
	for _, want := range []int{200, 404} {
		status.Store(int64(want))
		resp, data, _ = ts.do(alice, "HEAD", base+"/blobs/"+pushDigest("layer"), "", nil)
		expectStatus(t, "blob HEAD", resp, data, want)
	}
	resp, data, _ = ts.do(alice, "HEAD", strings.Replace(base, "/dev", "/Uppercase", 1)+"/blobs/"+pushDigest("layer"), "", nil)
	expectStatus(t, "environment", resp, data, 400)
	transport := ts.srv.push.client.Transport.(*http.Transport)
	if transport.Proxy != nil || ts.srv.push.client.Timeout <= 0 {
		t.Fatal("registry client is not isolated and bounded")
	}
}
