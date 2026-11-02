package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"brood.dev/go/brood/command"
)

// CommandExecutor binds catalog commands to an authenticated operator's scope.
// It preserves Client TLS, CSRF, redirect refusal, and credential handling.
type CommandExecutor struct {
	Client  *Client
	SwarmID string
}

// Execute posts canonical arguments to the server-owned command dispatcher.
func (e CommandExecutor) Execute(ctx context.Context, invocation command.Invocation) (command.Result, error) {
	if e.Client == nil {
		return command.Result{}, errors.New("operator client unavailable")
	}
	if e.SwarmID == "" && command.NeedsSwarm(invocation.Name) {
		return command.Result{}, errors.New("select a swarm with --swarm or BROOD_SWARM")
	}
	invocation, err := command.ParseTool(invocation.Name, invocation.Arguments)
	if err != nil {
		return command.Result{}, err
	}
	body, err := json.Marshal(struct {
		SwarmID   string          `json:"swarm_id"`
		Arguments json.RawMessage `json:"arguments"`
	}{e.SwarmID, invocation.Arguments})
	if err != nil {
		return command.Result{}, err
	}
	budget := 30 * time.Second
	if invocation.Name == "send" {
		var args struct {
			WaitSeconds int `json:"wait_seconds"`
		}
		if err := json.Unmarshal(invocation.Arguments, &args); err != nil {
			return command.Result{}, err
		}
		budget = max(budget, time.Duration(args.WaitSeconds)*time.Second+15*time.Second)
	}
	var result command.Result
	err = e.Client.callWithTimeout(ctx, http.MethodPost, "/api/commands/"+invocation.Name, body, &result, budget)
	return result, err
}
