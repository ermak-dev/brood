package kube

import (
	"context"
	"encoding/json"
	"net/url"
)

// PodCondition describes a scheduling/readiness condition without pod configuration.
type PodCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// PodWaiting describes why a worker container has not started.
type PodWaiting struct {
	Name    string `json:"name"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// PoolPodDiagnostic is the public status-only view of a worker pod.
type PoolPodDiagnostic struct {
	Name       string         `json:"name"`
	Phase      string         `json:"phase"`
	Conditions []PodCondition `json:"conditions"`
	Waiting    []PodWaiting   `json:"waiting"`
}

type diagnosticContainer struct {
	Name  string `json:"name"`
	State struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
	} `json:"state"`
}

type diagnosticPod struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Phase          string                `json:"phase"`
		Conditions     []PodCondition        `json:"conditions"`
		Containers     []diagnosticContainer `json:"containerStatuses"`
		InitContainers []diagnosticContainer `json:"initContainerStatuses"`
	} `json:"status"`
}

// Diagnostics lists only status fields of this pool's pods in the installation namespace.
func (p *Pools) Diagnostics(ctx context.Context, name string) ([]PoolPodDiagnostic, error) {
	if _, err := p.Get(ctx, name); err != nil {
		return nil, err
	}
	out := make([]PoolPodDiagnostic, 0)
	for token := ""; ; {
		query := url.Values{"labelSelector": []string{"ate.dev/worker-pool=" + name}, "limit": []string{"500"}}
		if token != "" {
			query.Set("continue", token)
		}
		response, err := p.client.request(ctx, "GET", "/api/v1/namespaces/"+Namespace+"/pods", query.Encode(), nil)
		if err != nil {
			return nil, poolsError(err)
		}
		items, ok := response["items"].([]any)
		if !ok {
			return nil, &KubeError{Status: 502, Detail: "Kubernetes returned an invalid worker pod list"}
		}
		for _, item := range items {
			raw, err := json.Marshal(item)
			if err != nil {
				return nil, &KubeError{Status: 502, Detail: "Kubernetes returned an invalid worker pod"}
			}
			var pod diagnosticPod
			if json.Unmarshal(raw, &pod) != nil || pod.Metadata.Name == "" {
				return nil, &KubeError{Status: 502, Detail: "Kubernetes returned an invalid worker pod"}
			}
			status := PoolPodDiagnostic{Name: pod.Metadata.Name, Phase: pod.Status.Phase, Conditions: pod.Status.Conditions, Waiting: make([]PodWaiting, 0)}
			for _, container := range append(pod.Status.InitContainers, pod.Status.Containers...) {
				if waiting := container.State.Waiting; waiting != nil {
					status.Waiting = append(status.Waiting, PodWaiting{Name: container.Name, Reason: waiting.Reason, Message: waiting.Message})
				}
			}
			out = append(out, status)
		}
		metadata, _ := response["metadata"].(map[string]any)
		next, _ := metadata["continue"].(string)
		if next == "" {
			return out, nil
		}
		token = next
	}
}
