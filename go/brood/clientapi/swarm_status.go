package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
)

// SwarmStatus is the durable swarm execution on the panel's legacy pipeline-run
// wire. Status uses waiting_human, not the store's waiting. No separate run is
// created. Source commits belong to retained pass-start nodes, not this object.
type SwarmStatus struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Definition  *SwarmDefinition  `json:"definition,omitempty"`
	Goal        string            `json:"goal,omitempty"`
	Status      string            `json:"status"`
	SwarmID     string            `json:"swarmId"`
	SwarmName   string            `json:"swarmName"`
	ProjectID   string            `json:"projectId"`
	ProjectName string            `json:"projectName"`
	Owner       string            `json:"owner"`
	StartedAt   *string           `json:"startedAt"`
	FinishedAt  *string           `json:"finishedAt,omitempty"`
	CurrentNode string            `json:"currentNode,omitempty"`
	Nodes       []SwarmStatusNode `json:"nodes"`
	Gate        *SwarmStatusGate  `json:"gate,omitempty"`
}

// SwarmStatusNode is a retained visit, not a unique graph node. Pass, Goal and
// SourceCommit may be absent from legacy history. Commit is workspace attestation,
// not a substitute for the pass's SourceCommit.
type SwarmStatusNode struct {
	ID           string  `json:"id"`
	Sequence     int64   `json:"sequence"`
	Kind         string  `json:"kind"`
	Status       string  `json:"status"`
	Label        string  `json:"label"`
	Outcome      string  `json:"outcome,omitempty"`
	ActorName    string  `json:"actorName,omitempty"`
	StartedAt    *string `json:"startedAt,omitempty"`
	FinishedAt   *string `json:"finishedAt,omitempty"`
	Commit       string  `json:"commit,omitempty"`
	Detail       string  `json:"detail,omitempty"`
	Report       string  `json:"report,omitempty"`
	Pass         int64   `json:"pass,omitempty"`
	Goal         string  `json:"goal,omitempty"`
	SourceCommit string  `json:"sourceCommit,omitempty"`
}

// SwarmStatusGate is the human gate offered by the server to this reader.
// Reading it neither answers the gate nor authorizes graph progression.
type SwarmStatusGate struct {
	RunID    string              `json:"runId"`
	NodeID   string              `json:"nodeId"`
	Sequence int64               `json:"visitSequence"`
	Question string              `json:"question"`
	Kind     string              `json:"kind"`
	Options  []SwarmStatusOption `json:"options"`
}

// SwarmStatusOption is one recorded gate option.
type SwarmStatusOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// SwarmStatus reads the existing execution endpoint once, without redirects,
// mutations or retries. HTTP error bodies and malformed payloads are not echoed.
// An unpinned swarm has no execution view and the server returns 404.
func (c *Client) SwarmStatus(ctx context.Context, swarm string) (SwarmStatus, error) {
	if swarm == "" || len(swarm) > 256 {
		return SwarmStatus{}, errors.New("a bounded swarm ID is required")
	}
	response, err := c.request(ctx, http.MethodGet, "/api/pipeline-runs/"+url.PathEscape(swarm), nil, 0, "", c.http.Timeout)
	if err != nil {
		return SwarmStatus{}, err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return SwarmStatus{}, ErrRedirect
	case response.StatusCode == http.StatusUnauthorized:
		return SwarmStatus{}, ErrSessionExpired
	case response.StatusCode != http.StatusOK:
		return SwarmStatus{}, fmt.Errorf("swarm status unavailable (status %d)", response.StatusCode)
	}
	const maxStatusBytes = 32 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxStatusBytes+1))
	if err != nil {
		return SwarmStatus{}, errors.New("could not read swarm status")
	}
	if len(data) > maxStatusBytes {
		return SwarmStatus{}, errors.New("swarm status exceeds 32 MiB")
	}
	var result SwarmStatus
	if json.Unmarshal(data, &result) != nil || !validSwarmStatus(result, swarm) {
		return SwarmStatus{}, errors.New("invalid swarm status response")
	}
	c.absorbCookies(response)
	return result, nil
}

func validSwarmStatus(view SwarmStatus, swarm string) bool {
	if view.ID != swarm || view.SwarmID != swarm || !slices.Contains([]string{"idle", "running", "waiting_human", "succeeded", "failed", "stopped"}, view.Status) || view.Nodes == nil {
		return false
	}
	if len(view.Nodes) == 0 {
		return view.Status == "idle" && view.CurrentNode == "" && view.Gate == nil
	}
	var sequence int64
	for _, node := range view.Nodes {
		if node.ID == "" || node.Sequence <= sequence || node.Pass < 0 || node.Pass > node.Sequence || !slices.Contains([]string{"pending", "active", "done", "failed", "completed", "unconfirmed", "refused", "stopped"}, node.Status) {
			return false
		}
		sequence = node.Sequence
	}
	last := view.Nodes[len(view.Nodes)-1]
	if view.CurrentNode != last.ID {
		return false
	}
	if gate := view.Gate; gate != nil && (gate.RunID != swarm || gate.NodeID != last.ID || gate.Sequence != last.Sequence) {
		return false
	}
	return true
}
