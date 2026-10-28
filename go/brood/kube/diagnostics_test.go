// FEATURE: Read-only namespaced diagnostics for worker pods.
// SPEC: Scope by pool label and return status only, never pod env or credentials.
package kube

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPoolDiagnosticsAreScopedAndSanitized(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("non-read request: %s", r.Method)
		}
		if r.URL.Path == collection+"/agents" {
			json.NewEncoder(w).Encode(pool())
			return
		}
		if r.URL.Path != "/api/v1/namespaces/brood/pods" || r.URL.Query().Get("labelSelector") != "ate.dev/worker-pool=agents" {
			t.Errorf("unscoped request: %s", r.URL)
			http.Error(w, "wrong path", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[{"metadata":{"name":"worker-2"},"spec":{"containers":[{"env":[{"name":"SECRET","value":"never-return-this"}]}]},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"Insufficient memory"}],"containerStatuses":[{"name":"worker","state":{"waiting":{"reason":"ContainerCreating"}}}]}}]}`))
	})
	result, err := api.pools().Diagnostics(t.Context(), "agents")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if len(result) != 1 || !strings.Contains(string(raw), "Insufficient memory") || !strings.Contains(string(raw), "worker-2") {
		t.Fatalf("diagnostics: %s", raw)
	}
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "never-return-this") || strings.Contains(string(raw), "spec") {
		t.Fatalf("leaked pod configuration: %s", raw)
	}
}

func TestPoolDiagnosticsRejectInvalidNamesAndMalformedCollections(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, collection+"/") {
			json.NewEncoder(w).Encode(pool())
			return
		}
		w.Write([]byte(`{"items":"invalid"}`))
	})
	if _, err := api.pools().Diagnostics(t.Context(), "../../secrets"); err == nil {
		t.Fatal("invalid pool name accepted")
	}
	if _, err := api.pools().Diagnostics(t.Context(), "agents"); err == nil {
		t.Fatal("malformed pod list accepted")
	}
}
