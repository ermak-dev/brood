package server

import (
	"context"
	"encoding/json"
	"fmt"

	"brood.dev/go/brood/proxmox/relay"
)

func (m *managedProvider) handleSwarmRequest(ctx context.Context, uid string, generation int64, req relay.SwarmRequest) relay.SwarmResponse {
	refused := relay.SwarmResponse{Text: "swarm operation refused"}
	from, err := m.server.store.GetManagedActor(ctx, uid)
	if err != nil || from.Incarnation != generation || from.Phase == "deleted" || from.Desired != "running" || from.Operation != "start" {
		return refused
	}
	swarm, err := m.server.store.GetSwarm(ctx, from.SwarmID)
	if err != nil || swarm.State != "active" || swarm.AtespaceUID == nil || *swarm.AtespaceUID != from.AtespaceUID {
		return refused
	}
	switch req.Tool {
	case "agents":
		actors, err := m.server.store.ListManagedActors(ctx, from.SwarmID)
		if err != nil {
			return refused
		}
		type actor struct {
			Name  string `json:"name"`
			State string `json:"state"`
		}
		roster := struct {
			Self   string  `json:"self"`
			Scope  string  `json:"scope"`
			Actors []actor `json:"actors"`
		}{Self: from.Name, Scope: "managed actors in this swarm", Actors: []actor{}}
		for _, a := range actors {
			if a.Phase != "deleted" {
				roster.Actors = append(roster.Actors, actor{Name: a.Name, State: a.Phase})
			}
		}
		data, err := json.Marshal(roster)
		if err != nil || len(data) > relay.MaxMessageBytes {
			return relay.SwarmResponse{Text: "swarm roster exceeds size limit"}
		}
		return relay.SwarmResponse{OK: true, Text: string(data)}
	case "send":
		to, err := m.server.store.LookupManagedActor(ctx, from.SwarmID, req.To)
		if err != nil || to.UID == from.UID {
			return relay.SwarmResponse{Text: "recipient is not another managed actor in this swarm"}
		}
		if m.hub == nil || !m.hub.Connected(to.UID, to.Incarnation) || m.deliverMessage == nil {
			return relay.SwarmResponse{Text: "recipient is not connected"}
		}
		body := req.Message + fmt.Sprintf("\n\n[brood swarm] Reply to the sender using mcp__brood__send(to=%q, message=...). Use mcp__brood__agents for managed actors in this swarm; built-in SendMessage does not reach them.", from.Name)
		response, err := m.deliverMessage(ctx, to.UID, to.Incarnation, relay.MessageRequest{To: to.Name, From: from.Name, Body: body, WaitSec: req.WaitSeconds})
		if err != nil {
			return relay.SwarmResponse{Text: "recipient did not confirm delivery"}
		}
		return relay.SwarmResponse{OK: response.OK, Text: response.Verdict}
	default:
		return refused
	}
}
