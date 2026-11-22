package server

import (
	"context"
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
	var api *app.APIError
	const prefix = "The agent was not created: "
	switch {
	case errors.As(err, &api) && api.Status < 500:
		return prefix + api.Detail
	case errors.As(err, &origin):
		return prefix + origin.detail
	case errors.Is(err, app.ErrNotAllowed):
		return prefix + "this master is no longer allowed to manage the swarm. Ask the operator."
	case errors.Is(err, app.ErrExists), store.IsUniqueViolation(err):
		return prefix + "the name is already taken in this swarm. Choose another name."
	case errors.Is(err, store.ErrPoolCapacity), errors.As(err, &capacity):
		return prefix + "the pool or the project has no room for one more agent. Delete an agent or ask the operator to raise the limit."
	case errors.Is(err, store.ErrPoolUnavailable):
		return prefix + "the pool does not take new agents. Ask the operator to enable it."
	case errors.Is(err, app.ErrInvalidWorkload):
		return prefix + "the name is not valid. Use lowercase letters, digits and dashes."
	case errors.Is(err, app.ErrConflict):
		return prefix + "the swarm changed while the command ran. Retry."
	default:
		return prefix + "the server or Proxmox is unavailable. Nothing changed; retry later."
	}
}

// authorizeMaster rechecks the durable UID/generation and its human approval.
func (m *managedProvider) authorizeMaster(ctx context.Context, original store.ManagedActor) error {
	a, err := m.server.store.GetManagedActor(ctx, original.UID)
	if err != nil || a.Incarnation != original.Incarnation || a.DelegatorID == "" || a.Phase != "running" || a.Desired != "running" || a.Operation != "start" || a.Stage != "done" {
		return app.ErrNotAllowed
	}
	u, err := m.server.store.GetUser(ctx, a.DelegatorID)
	if err != nil || u.Disabled || !u.IsAdmin {
		return app.ErrNotAllowed
	}
	return m.server.authorizeSwarmManagerIntent(ctx, u.ID, a.SwarmID)
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
	if req.Tool == command.GuestToolList {
		return m.guestToolList(ctx, from, swarm.ProjectID)
	}
	inv, err := command.ParseTool(req.Tool, req.Arguments)
	spec, ok := command.Lookup(req.Tool)
	if err != nil || !ok || !spec.Guest {
		return refused
	}
	return m.handleActorCommand(ctx, from, inv)
}

// sendFromAgent delivers a message of agent from to another agent of its
// swarm, or to the human owner of the swarm when to is operator.
func (m *managedProvider) sendFromAgent(ctx context.Context, from store.ManagedActor, name, message string, wait int) relay.SwarmResponse {
	if name == telegramOperator {
		// Адресат operator — владелец роя; канал к нему — Telegram.
		if err := m.server.telegramChannel.toOwner(ctx, from, message); err != nil {
			return relay.SwarmResponse{Text: "not delivered to the operator: " + err.Error()}
		}
		return relay.SwarmResponse{OK: true, Text: "delivered to the operator's Telegram chat"}
	}
	to, err := m.server.store.LookupManagedActor(ctx, from.SwarmID, name)
	if err != nil || to.UID == from.UID || to.Phase == "deleted" {
		return relay.SwarmResponse{Text: fmt.Sprintf("not sent: %q is not another agent of this swarm; list the agents with agents", name)}
	}
	if m.hub == nil || !m.hub.Connected(to.UID, to.Incarnation) || m.deliverMessage == nil {
		return relay.SwarmResponse{Text: fmt.Sprintf("not sent: agent %q is not connected; check its state with agents", name)}
	}
	body := message + fmt.Sprintf("\n\n[brood] This message is from agent %q of your swarm. To answer, use send with to=%q. The agents command lists the agents of the swarm.", from.Name, from.Name)
	response, err := m.deliverMessage(ctx, to.UID, to.Incarnation, relay.MessageRequest{To: to.Name, From: from.Name, Body: body, WaitSec: wait})
	if err != nil {
		return relay.SwarmResponse{Text: fmt.Sprintf("agent %q did not confirm the delivery; the message may have arrived, check before sending again", name)}
	}
	return relay.SwarmResponse{OK: response.OK, Text: response.Verdict}
}
