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

// Публичные исходы канала оператора к агенту: их текст читает клиент.
// Отключённый адресат — отказ команды: сообщение не отправлено.
var (
	errRecipientOffline    = command.Refuse(nil, "not sent: the agent is not connected; check its state with agents")
	errDeliveryUnconfirmed = command.Refuse(nil, "the agent did not confirm the delivery; the message may have arrived, check with tail before sending again")
)

// errMessageNotSent — гость ответил notSent (#244): сообщение до сессии
// Claude заведомо не дошло, повтор безопасен. Клиенту send не уходит:
// send показывает вердикт гостя, как прежде, ответ на запрос роя — failed.
var errMessageNotSent = errors.New("the agent refused the message before it was sent")

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
		return command.Result{}, command.Refuse(nil, "send needs to and message; nothing was sent")
	}
	byName := func(ctx context.Context) (store.ManagedActor, error) {
		return s.store.LookupManagedActor(ctx, swarmID, args.To)
	}
	result, _, err := s.deliverOperatorMessage(ctx, userID, swarmID, byName, args.Message, args.WaitSeconds)
	if errors.Is(err, errMessageNotSent) {
		// send сам не повторяет: оператор видит вердикт гостя.
		return result, nil
	}
	return result, err
}

// operatorMessageBody помечает сообщение оператора и называет адрес ответа:
// send на имя operator уходит владельцу роя.
func operatorMessageBody(message string) string {
	return message + "\n\n[brood] This message is from the human operator of your swarm. To answer, use send with to=\"operator\"."
}

// deliverOperatorMessage — канал оператора к актору роя: им идут send и
// ответ на запрос роя (#210). resolve выбирает адресата: send — по имени,
// ответ — спросившего актора той же инкарнации. Права проверяются до
// доставки и после неё; ошибка resolve — app.ErrNotFound. sent и ошибки —
// как у deliverToActor: отказ гостя до записи — errMessageNotSent.
func (s *Server) deliverOperatorMessage(ctx context.Context, userID, swarmID string, resolve func(context.Context) (store.ManagedActor, error), message string, wait int) (command.Result, bool, error) {
	user, err := s.store.GetUser(ctx, userID)
	if err != nil || user.Disabled {
		return command.Result{}, false, app.ErrNotAllowed
	}
	manager := func(context.Context) error {
		allowed, err := s.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(swarmID))
		if err != nil || !allowed {
			return app.ErrNotAllowed
		}
		return nil
	}
	return s.deliverToActor(ctx, swarmID, resolve, relay.MessageRequest{From: "operator:" + user.Username, Body: operatorMessageBody(message), WaitSec: wait}, manager)
}

// deliverToActor — общий канал сервера к актору роя: send и ответ на запрос
// оператора (#210), инструкция шага (#212). Адресат — актор этого роя в его
// нынешнем atespace, на связи relay; authorize — право отправителя, оно
// проверяется прямо перед отправкой и сразу после неё, вместе с
// неизменной инкарнацией адресата. sent сообщает, ушло ли сообщение в канал:
// при sent=false эффекта у актора нет, при sent=true с ошибкой исход
// неизвестен. Ответ гостя notSent — sent=false и errMessageNotSent с его
// вердиктом в Text, если права и адресат за время вызова не изменились.
// To заполняется именем адресата.
func (s *Server) deliverToActor(ctx context.Context, swarmID string, resolve func(context.Context) (store.ManagedActor, error), msg relay.MessageRequest, authorize func(context.Context) error) (command.Result, bool, error) {
	sw, err := s.store.GetSwarm(ctx, swarmID)
	if err != nil {
		return command.Result{}, false, app.ErrNotFound
	}
	target, err := resolve(ctx)
	if err != nil || target.SwarmID != sw.ID {
		return command.Result{}, false, app.ErrNotFound
	}
	if s.managed.hub == nil || !s.managed.hub.Connected(target.UID, target.Incarnation) || s.managed.deliverMessage == nil {
		return command.Result{}, false, errRecipientOffline
	}
	if err := authorize(ctx); err != nil {
		return command.Result{}, false, err
	}
	msg.To = target.Name
	actors, _ := ctx.Value(actorMessageScopeKey{}).([]string)
	actors = append(append([]string(nil), actors...), target.UID)
	ctx = context.WithValue(ctx, messageSubmissionKey{}, messageSubmission{actors: actors, authorize: authorize})
	response, err := s.managed.deliverMessage(ctx, target.UID, target.Incarnation, msg)
	// notSent вместе с OK противоречив: доставку подтверждает OK.
	unsent := err == nil && response.NotSent && !response.OK
	authErr := authorize(ctx)
	current, targetErr := s.store.GetManagedActor(ctx, target.UID)
	if authErr != nil || targetErr != nil || current.Incarnation != target.Incarnation {
		return command.Result{}, !unsent, app.ErrNotAllowed
	}
	if unsent {
		return command.Result{Text: response.Verdict, Outcome: "unconfirmed"}, false, errMessageNotSent
	}
	if err != nil {
		return command.Result{Outcome: "unconfirmed"}, true, errDeliveryUnconfirmed
	}
	outcome := "completed"
	if !response.OK {
		outcome = "unconfirmed"
	}
	return command.Result{Text: response.Verdict, Outcome: outcome}, true, nil
}
