package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// SwarmEvalSelection explicitly selects an immutable non-production evaluation.
type SwarmEvalSelection struct {
	ReferenceSwarmID string `json:"referenceSwarmId"`
	ReferencePass    int64  `json:"referencePass"`
	Definition       string `json:"definition"`
	Model            string `json:"model"`
}

// SwarmEvalReplay is a retained replay decision, not a historical human answer.
type SwarmEvalReplay struct {
	RequestID           string `json:"requestId"`
	State               string `json:"state"`
	Reason              string `json:"reason"`
	InterruptedChildren bool   `json:"interruptedChildren"`
}

// SwarmEval is the pinned binding and current retained evaluation status.
type SwarmEval struct {
	SwarmID          string           `json:"swarmId"`
	ReferenceSwarmID string           `json:"referenceSwarmId"`
	ReferencePass    int64            `json:"referencePass"`
	Definition       SwarmDefinition  `json:"definition"`
	Model            string           `json:"model"`
	Source           SwarmSource      `json:"source"`
	Goal             string           `json:"goal"`
	NonProduction    bool             `json:"nonProduction"`
	Execution        string           `json:"execution"`
	Replay           *SwarmEvalReplay `json:"replay"`
}

func validEvalName(value string) bool { return strings.TrimSpace(value) != "" && len(value) <= 256 }

// BindSwarmEval binds before candidate actors boot; it does not start a pass.
func (c *Client) BindSwarmEval(ctx context.Context, swarm string, selection SwarmEvalSelection) (SwarmEval, error) {
	if !validEvalName(swarm) || !validEvalName(selection.ReferenceSwarmID) || selection.ReferencePass < 1 || !validEvalName(selection.Definition) || !validEvalName(selection.Model) {
		return SwarmEval{}, errors.New("swarm, reference swarm, positive pass, definition and model are required")
	}
	body, err := json.Marshal(selection)
	if err != nil {
		return SwarmEval{}, err
	}
	var out SwarmEval
	if err = c.call(ctx, http.MethodPost, swarmExecutionPath(swarm, "eval"), body, &out); err != nil {
		return SwarmEval{}, err
	}
	return out, nil
}

// SwarmEval reads the retained binding and latest durable replay decision.
func (c *Client) SwarmEval(ctx context.Context, swarm string) (SwarmEval, error) {
	if !validEvalName(swarm) {
		return SwarmEval{}, errors.New("swarm is required")
	}
	var out SwarmEval
	if err := c.call(ctx, http.MethodGet, swarmExecutionPath(swarm, "eval"), nil, &out); err != nil {
		return SwarmEval{}, err
	}
	return out, nil
}

// StartSwarmEval starts only the bound selection, once, without overrides or retries.
func (c *Client) StartSwarmEval(ctx context.Context, swarm string) (SwarmExecution, error) {
	if !validEvalName(swarm) {
		return SwarmExecution{}, errors.New("swarm is required")
	}
	return c.swarmExecutionCall(ctx, swarmExecutionPath(swarm, "eval")+"/start", nil)
}
