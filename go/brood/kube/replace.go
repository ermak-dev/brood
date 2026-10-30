package kube

import (
	"context"
	"encoding/json"
)

// podLabel — метка, которой ate-controller помечает поды воркеров. Только они
// принадлежат пулу; соседей установки это действие не касается.
const podLabel = "ate.dev/worker-pool"

// ReplaceWorkerPod deletes a worker pod so its Deployment can create a new one.
// DRAINING is final: the old Worker record cannot become schedulable again.
// Deletion requires the observed pod UID and a worker-pool label; a replacement
// pod with the same name is protected by the Kubernetes UID precondition.
// Callers must verify that the worker is drained and has no actor allocations.
func (p *Pools) ReplaceWorkerPod(ctx context.Context, pod, uid string) error {
	name, err := validName(pod)
	if err != nil || name == "" || uid == "" {
		return &KubeError{Status: 400,
			Detail: "a worker pod name and the uid read with it are required"}
	}
	path := "/api/v1/namespaces/" + Namespace + "/pods/" + name
	current, err := p.client.request(ctx, "GET", path, "", nil)
	if err != nil {
		return poolsError(err)
	}
	metadata, _ := current["metadata"].(map[string]any)
	if metadata == nil || metadata["uid"] != uid {
		return &KubeError{Status: 409,
			Detail: "this worker pod has already been replaced; reload the workers before retrying"}
	}
	labels, _ := metadata["labels"].(map[string]any)
	if value, _ := labels[podLabel].(string); value == "" {
		return &KubeError{Status: 400,
			Detail: "that pod does not belong to a worker pool"}
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion":    "v1",
		"kind":          "DeleteOptions",
		"preconditions": map[string]any{"uid": uid},
	})
	if err != nil {
		return &KubeError{Status: 500, Detail: "worker pod delete options could not be built"}
	}
	if _, err := p.client.request(ctx, "DELETE", path, "", body); err != nil {
		return poolsError(err)
	}
	return nil
}
