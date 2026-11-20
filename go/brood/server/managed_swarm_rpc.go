package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

var managedActorName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

func managedAddRefusal(err error) string {
	var capacity *app.CapacityExceededError
	var origin *originRefusal
	switch {
	case errors.As(err, &origin):
		return "actor creation refused: " + origin.detail
	case errors.Is(err, app.ErrNotAllowed):
		return "actor creation refused: master delegation is not authorized"
	case errors.Is(err, app.ErrExists), store.IsUniqueViolation(err):
		return "actor creation refused: actor name or allocation is already in use"
	case errors.Is(err, store.ErrPoolCapacity), errors.As(err, &capacity):
		return "actor creation refused: pool or project capacity exhausted"
	case errors.Is(err, store.ErrPoolUnavailable):
		return "actor creation refused: managed pool is not enabled"
	case errors.Is(err, app.ErrInvalidWorkload):
		return "actor creation refused: invalid workload or required worker selector"
	case errors.Is(err, app.ErrConflict):
		return "actor creation refused: managed runtime or namespace binding conflict"
	default:
		return "actor creation refused: managed resource or provider unavailable"
	}
}

// authorizeMaster rechecks the durable UID/generation and its human approval.
func (m *managedProvider) authorizeMaster(ctx context.Context, original store.ManagedActor) error {
	a, err := m.server.store.GetManagedActor(ctx, original.UID)
	if err != nil || a.Incarnation != original.Incarnation || a.Role != "master" || a.DelegatorID == "" || a.Phase != "running" || a.Desired != "running" || a.Operation != "start" || a.Stage != "done" {
		return app.ErrNotAllowed
	}
	u, err := m.server.store.GetUser(ctx, a.DelegatorID)
	if err != nil || u.Disabled || !u.IsAdmin {
		return app.ErrNotAllowed
	}
	allowed, err := m.server.app.Can(u.ID, app.ActionManageSwarm, app.SwarmRes(a.SwarmID))
	if err != nil {
		return err
	}
	if !allowed {
		return app.ErrNotAllowed
	}
	return nil
}

func (m *managedProvider) handleSwarmRequest(ctx context.Context, uid string, generation int64, req relay.SwarmRequest) relay.SwarmResponse {
	refused := relay.SwarmResponse{Text: "swarm operation refused"}
	from, err := m.server.store.GetManagedActor(ctx, uid)
	if err != nil || from.Incarnation != generation || from.Phase == "deleted" || from.Desired != "running" || from.Operation != "start" {
		return refused
	}
	swarm, err := m.server.store.GetSwarm(ctx, from.SwarmID)
	if err != nil || swarm.State != "active" {
		return refused
	}
	if len(req.Arguments) != 0 {
		inv, err := command.ParseTool(req.Tool, req.Arguments)
		spec, ok := command.Lookup(req.Tool)
		if err != nil || !ok || !spec.Guest {
			return refused
		}
		return m.handleActorCommand(ctx, from, inv)
	}
	switch req.Tool {
	case "add":
		args, _ := json.Marshal(map[string]string{"name": req.To})
		result, err := m.executeLifecycleCommand(ctx, from, command.Invocation{Name: "add", Arguments: args})
		if err != nil {
			return relay.SwarmResponse{Text: err.Error()}
		}
		return commandSwarmResponse(result)
	case "agents":
		actors, err := m.server.store.ListManagedActors(ctx, from.SwarmID)
		if err != nil {
			return refused
		}
		type actor struct {
			Name  string `json:"name"`
			State string `json:"state"`
			Role  string `json:"role"`
		}
		roster := struct {
			Self   string  `json:"self"`
			Scope  string  `json:"scope"`
			Actors []actor `json:"actors"`
		}{Self: from.Name, Scope: "managed actors in this swarm; send to operator reaches the human owner of the swarm", Actors: []actor{}}
		for _, a := range actors {
			if a.Phase != "deleted" {
				roster.Actors = append(roster.Actors, actor{Name: a.Name, State: a.Phase, Role: a.Role})
			}
		}
		data, err := json.Marshal(roster)
		if err != nil || len(data) > relay.MaxMessageBytes {
			return relay.SwarmResponse{Text: "swarm roster exceeds size limit"}
		}
		return relay.SwarmResponse{OK: true, Text: string(data)}
	case "send":
		if req.To == telegramOperator {
			// Адресат operator — владелец роя; канал к нему — Telegram.
			if err := m.server.telegramChannel.toOwner(ctx, from, req.Message); err != nil {
				return relay.SwarmResponse{Text: "not delivered to the operator: " + err.Error()}
			}
			return relay.SwarmResponse{OK: true, Text: "delivered to the operator's Telegram chat"}
		}
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
