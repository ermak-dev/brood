// BUG: a drained worker was a dead end -- the panel could strand the whole
// installation and only kubectl could bring the capacity back (#89).
// SPEC: Administrators may replace a worker the scheduler has given up on.
// A worker still taking actors is never replaced from here, and an ordinary
// user never reaches Kubernetes.
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

	ateapi "go.ate.dev/substrate/api/ateapi/v1"

	"brood.dev/go/brood/kube"
)

// kubeFor подключает к серверу поддельный Kubernetes и считает удаления подов.
func kubeFor(t *testing.T, ts *substrateServer) *atomic.Int32 {
	t.Helper()
	deletes := &atomic.Int32{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"metadata":{"name":"pool-1","uid":"pod-uid","labels":{"ate.dev/worker-pool":"pool"}}}`))
	}))
	t.Cleanup(upstream.Close)
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
	return deletes
}

func worker(state ateapi.WorkerState) *ateapi.Worker {
	return &ateapi.Worker{
		Metadata: fakeMeta("worker-1", ""), WorkerPool: "pool",
		WorkerNamespace: kube.Namespace, WorkerPod: "pool-1", WorkerPodUid: "pod-uid",
		Status: &ateapi.WorkerStatus{State: state},
	}
}

func TestReplaceDrainedWorkerReturnsCapacity(t *testing.T) {
	fake := newFakeAteapi()
	fake.mu.Lock()
	fake.workers["worker-1"] = worker(ateapi.WorkerState_WORKER_STATE_DRAINING)
	fake.mu.Unlock()
	ts := newSubstrateServer(t, fake)
	admin, alice, _, _ := actorsSetup(t, ts.testServer)
	deletes := kubeFor(t, ts)

	resp, data, _ := ts.do(alice, "POST", "/api/workers/worker-1", ts.csrf(alice),
		map[string]string{"action": "replace"})
	expectStatus(t, "ordinary user replace", resp, data, http.StatusForbidden)
	if deletes.Load() != 0 {
		t.Fatal("an ordinary user reached Kubernetes")
	}

	resp, data, _ = ts.do(admin, "POST", "/api/workers/worker-1", ts.csrf(admin),
		map[string]string{"action": "replace"})
	expectStatus(t, "replace drained worker", resp, data, http.StatusNoContent)
	if deletes.Load() != 1 {
		t.Fatalf("pod deletions: %d, want 1", deletes.Load())
	}
}

func TestReplaceRefusesADrainedWorkerWithAllocations(t *testing.T) {
	for _, allocation := range []*ateapi.WorkerResources{
		{Actors: 1}, {CpuMillicores: 1}, {MemoryBytes: 1},
	} {
		t.Run(allocation.String(), func(t *testing.T) {
			fake := newFakeAteapi()
			item := worker(ateapi.WorkerState_WORKER_STATE_DRAINING)
			item.Status.Allocated = allocation
			fake.workers["worker-1"] = item
			ts := newSubstrateServer(t, fake)
			admin, _, _, _ := actorsSetup(t, ts.testServer)
			deletes := kubeFor(t, ts)
			resp, data, _ := ts.do(admin, "POST", "/api/workers/worker-1", ts.csrf(admin),
				map[string]string{"action": "replace"})
			expectStatus(t, "allocated worker", resp, data, http.StatusConflict)
			if deletes.Load() != 0 {
				t.Fatal("replacement would interrupt an allocated actor")
			}
		})
	}
}

func TestReplaceRefusesAWorkerThatStillTakesActors(t *testing.T) {
	fake := newFakeAteapi()
	fake.mu.Lock()
	fake.workers["worker-1"] = worker(ateapi.WorkerState_WORKER_STATE_ACTIVE)
	fake.mu.Unlock()
	ts := newSubstrateServer(t, fake)
	admin, _, _, _ := actorsSetup(t, ts.testServer)
	deletes := kubeFor(t, ts)

	resp, data, _ := ts.do(admin, "POST", "/api/workers/worker-1", ts.csrf(admin),
		map[string]string{"action": "replace"})
	expectStatus(t, "replace active worker", resp, data, http.StatusConflict)
	if deletes.Load() != 0 {
		t.Fatal("replaced a worker that still takes actors")
	}
	detail, _ := data["detail"].(string)
	if !strings.Contains(detail, "drain") {
		t.Fatalf("refusal does not say what to do first: %v", data)
	}
}
