package server

import (
	"context"
	"encoding/json"
	"errors"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// Публичные исходы канала оператора к актору: их текст читает клиент.
var (
	errRecipientOffline    = errors.New("recipient is not connected")
	errDeliveryUnconfirmed = errors.New("message delivery is unconfirmed; check before retrying")
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
	byName := func(ctx context.Context) (store.ManagedActor, error) {
		return s.store.LookupManagedActor(ctx, swarmID, args.To)
	}
	return s.deliverOperatorMessage(ctx, userID, swarmID, byName, args.Message, args.WaitSeconds)
}

// operatorMessageBody помечает сообщение оператора: адрес отправителя — не
// актор, ответить ему через send нельзя.
func operatorMessageBody(message string) string {
	return message + "\n\n[brood] This message is from a human operator, not an actor reply address. Replying to operator names through actor send is not supported."
}

// deliverOperatorMessage — канал оператора к актору роя: им идут send и
// ответ на запрос роя (#210). resolve выбирает адресата: send — по имени,
// ответ — спросившего актора той же инкарнации. Права проверяются до
// доставки и после неё; ошибка resolve — app.ErrNotFound.
func (s *Server) deliverOperatorMessage(ctx context.Context, userID, swarmID string, resolve func(context.Context) (store.ManagedActor, error), message string, wait int) (command.Result, error) {
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
	target, err := resolve(ctx)
	if err != nil || target.SwarmID != sw.ID || sw.AtespaceUID == nil || target.AtespaceUID != *sw.AtespaceUID {
		return command.Result{}, app.ErrNotFound
	}
	if s.managed.hub == nil || !s.managed.hub.Connected(target.UID, target.Incarnation) || s.managed.deliverMessage == nil {
		return command.Result{}, errRecipientOffline
	}
	response, err := s.managed.deliverMessage(ctx, target.UID, target.Incarnation, relay.MessageRequest{To: target.Name, From: "operator:" + user.Username, Body: operatorMessageBody(message), WaitSec: wait})
	allowed, authErr := s.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(swarmID))
	current, targetErr := s.store.GetManagedActor(ctx, target.UID)
	if authErr != nil || !allowed || targetErr != nil || current.Incarnation != target.Incarnation {
		return command.Result{}, app.ErrNotAllowed
	}
	if err != nil {
		return command.Result{Outcome: "unconfirmed"}, errDeliveryUnconfirmed
	}
	outcome := "completed"
	if !response.OK {
		outcome = "unconfirmed"
	}
	return command.Result{Text: response.Verdict, Outcome: outcome}, nil
}
