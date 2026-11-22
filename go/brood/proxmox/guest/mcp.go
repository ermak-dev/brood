//go:build linux

package guest

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/mcp"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/swarmclient"
)

// ServeMCP is the supervisor entry point of the catalog-backed MCP lifecycle. The supplied caller remains the only authority.
func ServeMCP(ctx context.Context, input io.Reader, output io.Writer, call relay.SwarmCaller) error {
	return mcp.Serve(ctx, input, output, relayExecutor{call: call}, mcp.GuestSpecs())
}

type relayExecutor struct{ call relay.SwarmCaller }

func (e relayExecutor) ListGuestTools(ctx context.Context) ([]string, error) {
	if e.call == nil {
		return nil, errors.New("swarm broker unavailable")
	}
	result := e.call(ctx, relay.SwarmRequest{Tool: command.GuestToolList, Arguments: json.RawMessage(`{}`)})
	if !result.OK {
		return nil, errors.New(result.Text)
	}
	var tools []string
	err := json.Unmarshal(result.Data, &tools)
	return tools, err
}

func (e relayExecutor) Execute(ctx context.Context, invocation command.Invocation) (command.Result, error) {
	request, err := swarmclient.NewRequest(invocation)
	if err != nil {
		return command.Result{}, err
	}
	if e.call == nil {
		return command.Result{}, errors.New("swarm broker unavailable")
	}
	wireRequest, _ := json.Marshal(relay.SwarmRequest{Tool: request.Tool, Arguments: request.Arguments})
	validated, valid := relay.DecodeSwarmRequest(wireRequest)
	if !valid {
		return command.Result{}, mcp.ErrInvalidArguments
	}
	result := e.call(ctx, validated)
	// Preserve optional result fields added by the transport without coupling
	// this executor to the supervisor implementation.
	wire, _ := json.Marshal(result)
	var converted command.Result
	_ = json.Unmarshal(wire, &converted)
	if !result.OK {
		return converted, errors.New(result.Text)
	}
	return converted, nil
}
