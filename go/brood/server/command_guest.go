package server

import (
	"context"
	"encoding/json"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

func commandSwarmResponse(result command.Result) relay.SwarmResponse {
	ok := true
	switch result.Outcome {
	case "unconfirmed", "unknown", "failed", "refused", "unsupported":
		ok = false
	}
	if result.Text == "" {
		result.Text = "(empty)"
	}
	return relay.SwarmResponse{OK: ok, Text: result.Text, Data: result.Data, Outcome: result.Outcome}
}

// handleActorCommand executes a catalog command called by an agent. from is
// the calling agent as the relay authenticated it.
func (m *managedProvider) handleActorCommand(ctx context.Context, from store.ManagedActor, inv command.Invocation) relay.SwarmResponse {
	switch inv.Name {
	case "diagnose":
		data, _ := json.Marshal(map[string]any{"mode": "agent", "self": from.Name, "role": from.Role, "generation": from.Incarnation, "swarm_id": from.SwarmID, "relay": "connected"})
		return relay.SwarmResponse{OK: true, Text: string(data), Data: data, Outcome: "completed"}
	case "tail", "slash", "restart":
		result, err := m.executeGuestSessionCommand(ctx, from, inv)
		if err != nil {
			return relay.SwarmResponse{Text: err.Error(), Outcome: result.Outcome}
		}
		return commandSwarmResponse(result)
	case "agents":
		result, err := m.server.agentRoster(ctx, from.SwarmID, from.Name)
		if err != nil {
			return relay.SwarmResponse{Text: err.Error()}
		}
		return commandSwarmResponse(result)
	case "add", "rm", "stop", "start", "reset":
		result, err := m.executeLifecycleCommand(ctx, from, inv)
		if err != nil {
			return relay.SwarmResponse{Text: err.Error(), Outcome: result.Outcome}
		}
		return commandSwarmResponse(result)
	case "send":
		var args struct {
			To          string `json:"to"`
			Message     string `json:"message"`
			WaitSeconds int    `json:"wait_seconds"`
		}
		_ = json.Unmarshal(inv.Arguments, &args)
		return m.sendFromAgent(ctx, from, args.To, args.Message, args.WaitSeconds)
	}
	return relay.SwarmResponse{Text: "unknown command"}
}
