// BUG: WORKER_STATE_DRAINING is final, and nothing in brood brought a drained
// worker's capacity back, so a panel click could strand the installation (#89).
// SPEC: Replacing a drained worker deletes exactly its own pod, under a uid
// precondition, and only for a pod that belongs to a worker pool. The
// Deployment then creates a fresh pod whose worker registers schedulable.
package kube

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const podPath = "/api/v1/namespaces/brood/pods/agents-1"

func workerPod(uid string, labels map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": "agents-1", "uid": uid, "labels": labels}}
}

func TestReplaceWorkerPodDeletesOnlyThatPodUnderItsUID(t *testing.T) {
	deleted := 0
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != podPath {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method == http.MethodDelete {
			deleted++
		}
		json.NewEncoder(w).Encode(workerPod("pod-uid", map[string]any{"ate.dev/worker-pool": "agents"}))
	})
	if err := api.pools().ReplaceWorkerPod(t.Context(), "agents-1", "pod-uid"); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("pod deletions: %d, want 1", deleted)
	}
	api.mu.Lock()
	body := strings.Join(api.sent, " ")
	api.mu.Unlock()
	if !strings.Contains(body, `"uid":"pod-uid"`) || !strings.Contains(body, "preconditions") {
		t.Fatalf("delete was not bound to the pod uid: %s", body)
	}
}

func TestReplaceWorkerPodRefusesAPodThatIsNoLongerTheRecordedOne(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			t.Error("deleted a pod that is not the recorded one")
		}
		json.NewEncoder(w).Encode(workerPod("replacement-uid", map[string]any{"ate.dev/worker-pool": "agents"}))
	})
	err := api.pools().ReplaceWorkerPod(t.Context(), "agents-1", "pod-uid")
	if err == nil || !strings.Contains(err.Error(), "already been replaced") {
		t.Fatalf("stale uid accepted: %v", err)
	}
}

func TestReplaceWorkerPodRefusesAPodOutsideAnyWorkerPool(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			t.Error("deleted a pod that belongs to no worker pool")
		}
		json.NewEncoder(w).Encode(workerPod("pod-uid", map[string]any{"app": "brood-go"}))
	})
	err := api.pools().ReplaceWorkerPod(t.Context(), "agents-1", "pod-uid")
	if err == nil || !strings.Contains(err.Error(), "worker pool") {
		t.Fatalf("neighbouring pod accepted: %v", err)
	}
}

func TestReplaceWorkerPodRejectsNamesThatLeaveTheCollection(t *testing.T) {
	api := startAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an invalid pod name reached Kubernetes")
		w.WriteHeader(500)
	})
	for _, name := range []string{"../../secrets", "", "Agents-1"} {
		if err := api.pools().ReplaceWorkerPod(t.Context(), name, "pod-uid"); err == nil {
			t.Fatalf("invalid pod name accepted: %q", name)
		}
	}
	if err := api.pools().ReplaceWorkerPod(t.Context(), "agents-1", ""); err == nil {
		t.Fatal("empty uid accepted")
	}
}
