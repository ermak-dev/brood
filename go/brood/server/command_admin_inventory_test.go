package server

// FEATURE: WorkerPool inventory shares provider-qualified node output.
// SPEC: Native infrastructure credentials and configuration never enter the DTO.

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/kube"
)

func TestAdminCommandNativePoolInventory(t *testing.T) {
	s := adminCommandServer(t)
	var calls atomic.Int32
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"items":[{"metadata":{"name":"native-pool","annotations":{"credential":"do-not-return"}},"spec":{"endpoint":"https://private-pool.invalid","token":"do-not-return"}}],"metadata":{}}`)
	}))
	defer backend.Close()
	ca := writeFileFake(t, "pool-ca", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})))
	pool, err := kube.NewPools(backend.URL, writeFileFake(t, "pool-token", "do-not-return"), ca, `{"gvisor":"registry/worker@sha256:`+strings.Repeat("a", 64)+`"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	s.pools = pool
	if _, err := runAdminCommand(s, "ordinary", "", "node", `{}`); err == nil {
		t.Fatal("ordinary inventory allowed")
	}
	if calls.Load() != 0 {
		t.Fatal("ordinary user reached Kubernetes")
	}
	result, err := runAdminCommand(s, "admin", "", "node", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	text := string(result.Data)
	for _, want := range []string{`"name":"native:native-pool"`, `"kind":"WorkerPool"`, `"native_pools":"available"`} {
		if !strings.Contains(text, want) {
			t.Errorf("inventory missing %s: %s", want, text)
		}
	}
	for _, value := range []string{"do-not-return", "private-pool.invalid", "credential", "endpoint"} {
		if strings.Contains(text, value) {
			t.Fatalf("pool configuration leaked: %s", text)
		}
	}
}
