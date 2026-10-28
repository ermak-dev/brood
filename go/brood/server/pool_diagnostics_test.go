// FEATURE: Administrator-only read-only worker pool diagnostics.
// SPEC: Return scheduling status without pod configuration; deny ordinary users before upstream access.
package server

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/kube"
)

func TestPoolDiagnosticsHTTP(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "password")
	admin := newClient(t)
	ts.login(admin, "root", "password")
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/workerpools/") {
			w.Write([]byte(`{"metadata":{"name":"agents"}}`))
			return
		}
		if r.URL.Path != "/api/v1/namespaces/brood/pods" || r.URL.Query().Get("labelSelector") != "ate.dev/worker-pool=agents" {
			t.Errorf("unscoped request: %s", r.URL)
		}
		w.Write([]byte(`{"items":[{"metadata":{"name":"pending"},"spec":{"secret":"never-return-this"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","message":"Insufficient memory"}]}}]}`))
	}))
	defer upstream.Close()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	ca := filepath.Join(dir, "ca.pem")
	os.WriteFile(token, []byte("test-token"), 0600)
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600)
	pools, err := kube.NewPools(upstream.URL, token, ca, `{"gvisor":"registry/worker@sha256:`+strings.Repeat("a", 64)+`"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	ts.srv.pools = pools
	path := "/api/worker-pools/agents/diagnostics"
	r, data, raw := ts.do(admin, "GET", path, "", nil)
	expectStatus(t, "diagnostics", r, data, 200)
	if !strings.Contains(raw, "Insufficient memory") || strings.Contains(raw, "never-return-this") || strings.Contains(raw, "test-token") {
		t.Fatalf("unsafe diagnostics: %s", raw)
	}
	ts.do(admin, "POST", "/api/admin/users", ts.csrf(admin), map[string]string{"username": "member", "password": "password"})
	member := newClient(t)
	ts.login(member, "member", "password")
	before := calls.Load()
	r, data, _ = ts.do(member, "GET", path, "", nil)
	expectStatus(t, "ordinary user", r, data, 403)
	r, data, _ = ts.do(newClient(t), "GET", path, "", nil)
	expectStatus(t, "anonymous", r, data, 401)
	if calls.Load() != before {
		t.Fatal("unauthorized request reached Kubernetes")
	}
}
