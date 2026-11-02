package server

import (
	"context"
	"encoding/json"
	"errors"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
)

func (s *Server) sendOperatorCommand(ctx context.Context, userID, swarmID string, inv command.Invocation) (command.Result, error) {
	allowed, err := s.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(swarmID))
	if err != nil {
		return command.Result{}, err
	}
	if !allowed {
		return command.Result{}, app.ErrNotAllowed
	}
	var args struct {
		To          string `json:"to"`
		Message     string `json:"message"`
		WaitSeconds int    `json:"wait_seconds"`
	}
	if json.Unmarshal(inv.Arguments, &args) != nil {
		return command.Result{}, errors.New("invalid send arguments")
	}
	user, err := s.store.GetUser(ctx, userID)
	if err != nil || user.Disabled {
		return command.Result{}, app.ErrNotAllowed
	}
	sw, err := s.store.GetSwarm(ctx, swarmID)
	if err != nil {
		return command.Result{}, app.ErrNotFound
	}
	if err := s.checkSwarmNamespace(ctx, toAppSwarm(sw)); err != nil {
		return command.Result{}, err
	}
	target, err := s.store.LookupManagedActor(ctx, swarmID, args.To)
	if err != nil || sw.AtespaceUID == nil || target.AtespaceUID != *sw.AtespaceUID {
		return command.Result{}, app.ErrNotFound
	}
	if s.managed.hub == nil || !s.managed.hub.Connected(target.UID, target.Incarnation) || s.managed.deliverMessage == nil {
		return command.Result{}, errors.New("recipient is not connected")
	}
	response, err := s.managed.deliverMessage(ctx, target.UID, target.Incarnation, relay.MessageRequest{To: target.Name, From: "operator:" + user.Username, Body: args.Message + "\n\n[brood] This message is from a human operator, not an actor reply address. Replying to operator names through actor send is not supported.", WaitSec: args.WaitSeconds})
	allowed, authErr := s.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(swarmID))
	current, targetErr := s.store.GetManagedActor(ctx, target.UID)
	if authErr != nil || !allowed || targetErr != nil || current.Incarnation != target.Incarnation {
		return command.Result{}, app.ErrNotAllowed
	}
	if err != nil {
		return command.Result{Outcome: "unconfirmed"}, errors.New("message delivery is unconfirmed; check before retrying")
	}
	outcome := "completed"
	if !response.OK {
		outcome = "unconfirmed"
	}
	return command.Result{Text: response.Verdict, Outcome: outcome}, nil
}
