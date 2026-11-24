package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
)

// Роутер сообщений роя (#115): авторизованный гость адресует актора по имени
// своего роя. Проверка соседства — на сервере: гость не решает, кому можно.

// deliverMessage открывает канал brood-swarm-msg в гостя-получателя.
// Подменяется в тестах; производство держит hub.Dial.
type messageDeliverer func(ctx context.Context, toUID string, toGeneration int64, req relay.MessageRequest) (relay.MessageResponse, error)

// enableSwarmMessages подключает роутер к relay-хабу после initialize.
func (m *provider) enableSwarmMessages() {
	if m.hub == nil {
		return
	}
	if m.deliverMessage == nil {
		m.deliverMessage = m.deliverViaHub
	}
	m.hub.SetMessageRouter(m.swarmMessageRouter())
	if hub, ok := m.hub.(interface{ SetSwarmHandler(relay.SwarmHandler) }); ok {
		hub.SetSwarmHandler(m.handleSwarmRequest)
	}
}

// swarmMessageRouter возвращает роутер: отправитель уже авторизован хабом,
// здесь проверяется адресат (тот же рой, живой актор) и выполняется доставка.
func (m *provider) swarmMessageRouter() relay.MessageRouter {
	return func(fromUID string, req relay.MessageRequest) relay.MessageResponse {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		from, err := m.server.store.GetAgent(ctx, fromUID)
		if err != nil || from.Phase == "deleted" {
			return relay.MessageResponse{OK: false, Verdict: "sender is not an active actor"}
		}
		if req.From != from.Name {
			return relay.MessageResponse{OK: false, Verdict: "from does not match the sending actor"}
		}
		if req.To == telegramOperator {
			// Адресат operator — владелец роя; канал к нему — Telegram.
			if m.server.telegramChannel == nil {
				return relay.MessageResponse{OK: false, Verdict: "the operator channel is not configured"}
			}
			if err := m.server.telegramChannel.toOwner(ctx, from, req.Body); err != nil {
				return relay.MessageResponse{OK: false, Verdict: "not delivered to the operator: " + err.Error()}
			}
			return relay.MessageResponse{OK: true, Verdict: "delivered to the operator's Telegram chat"}
		}
		to, err := m.server.store.LookupAgent(ctx, from.SwarmID, req.To)
		if err != nil {
			return relay.MessageResponse{OK: false, Verdict: fmt.Sprintf("no actor %s in this swarm", req.To)}
		}
		if to.UID == from.UID {
			return relay.MessageResponse{OK: false, Verdict: "cannot message itself"}
		}
		if m.hub == nil || !m.hub.Connected(to.UID, to.Incarnation) {
			return relay.MessageResponse{OK: false, Verdict: "recipient is not connected"}
		}
		deliver := m.deliverMessage
		if deliver == nil {
			return relay.MessageResponse{OK: false, Verdict: "recipient is not connected"}
		}
		// Контекст доставки выводится от Background: WaitSec до 300 с —
		// обещание контракта, ребёнок 30-секундного роутерного контекста
		// урезал бы его молча. Общий потолок остаётся конечным: предел
		// канала, без ожидания простоя — с квитанцией адресата (#260).
		dctx, dcancel := context.WithTimeout(context.Background(), command.CallTimeout(req.WaitSec))
		defer dcancel()
		resp, err := deliver(dctx, to.UID, to.Incarnation, req)
		if err != nil || !resp.OK {
			verdict := resp.Verdict
			if verdict == "" {
				verdict = "recipient did not confirm delivery"
			}
			return relay.MessageResponse{OK: false, Verdict: verdict}
		}
		return resp
	}
}

// deliverViaHub — производство messageDeliverer: канал сообщений в гостя.
func (m *provider) deliverViaHub(ctx context.Context, toUID string, toGeneration int64, req relay.MessageRequest) (relay.MessageResponse, error) {
	if m.hub == nil {
		return relay.MessageResponse{}, fmt.Errorf("relay hub unavailable")
	}
	var release func()
	if submission, ok := ctx.Value(messageSubmissionKey{}).(messageSubmission); ok {
		var err error
		release, err = m.effects.acquire(ctx, submission.actors...)
		if err != nil {
			return relay.MessageResponse{NotSent: true, Verdict: "Message submission was cancelled before opening the guest channel."}, nil
		}
		defer func() {
			if release != nil {
				release()
			}
		}()
		if err := submission.authorize(ctx); err != nil {
			return relay.MessageResponse{NotSent: true, Verdict: "Message is no longer authorized; no guest channel was opened."}, nil
		}
	}
	frame, err := json.Marshal(req)
	if err != nil {
		return relay.MessageResponse{}, err
	}
	conn, err := m.hub.DialMessage(ctx, toUID, toGeneration, frame)
	if release != nil {
		release()
		release = nil
	}
	if err != nil {
		return relay.MessageResponse{}, err
	}
	defer conn.Close()
	return relay.ReadMessageResponse(conn)
}
