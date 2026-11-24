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
func (m *provider) handleActorCommand(ctx context.Context, from store.Agent, inv command.Invocation) relay.SwarmResponse {
	if inv.Name == "agents" || inv.Name == "diagnose" || inv.Name == "artifact_publish" || inv.Name == "artifact_get" {
		if err := m.authorizeGuestPermission(ctx, from, inv.Name); err != nil {
			return relay.SwarmResponse{Text: err.Error()}
		}
	}
	switch inv.Name {
	case "diagnose":
		data, _ := json.Marshal(map[string]any{"mode": "agent", "self": from.Name, "role": m.server.agentRoleName(ctx, from.Role), "generation": from.Incarnation, "swarm_id": from.SwarmID, "relay": "connected"})
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
	case "artifact_publish", "artifact_get":
		result, err := m.server.executeArtifactCommand(ctx, from.SwarmID, inv)
		if err != nil {
			return relay.SwarmResponse{Text: err.Error(), Outcome: result.Outcome}
		}
		return commandSwarmResponse(result)
	case "send":
		var args struct {
			To                string `json:"to"`
			Message           string `json:"message"`
			WaitSeconds       int    `json:"wait_seconds"`
			ArtifactVersionID string `json:"artifact_version_id"`
		}
		_ = json.Unmarshal(inv.Arguments, &args)
		permission := "send:agents"
		if args.To == telegramOperator {
			permission = "send:operator"
		}
		if err := m.authorizeGuestPermission(ctx, from, permission); err != nil {
			return relay.SwarmResponse{Text: err.Error()}
		}
		if args.ArtifactVersionID != "" {
			if args.To != telegramOperator {
				return relay.SwarmResponse{Text: "artifact_version_id is only for operator notifications; pass the version ID in the assignment to an agent"}
			}
			if err := m.server.telegramChannel.sendArtifact(ctx, from, args.Message, args.ArtifactVersionID); err != nil {
				return relay.SwarmResponse{Text: err.Error()}
			}
			return relay.SwarmResponse{OK: true, Text: "artifact notice delivered to the operator; waiting for a reply is separate"}
		}
		return m.sendFromAgent(ctx, from, args.To, args.Message, args.WaitSeconds)
	}
	return relay.SwarmResponse{Text: "unknown command"}
}
