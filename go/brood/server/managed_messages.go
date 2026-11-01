package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

// Роутер сообщений роя (#115): авторизованный гость адресует актора по имени
// своего роя. Проверка соседства — на сервере: гость не решает, кому можно.

// deliverMessage открывает канал brood-swarm-msg в гостя-получателя.
// Подменяется в тестах; производство держит hub.Dial.
type messageDeliverer func(ctx context.Context, toUID string, toGeneration int64, req relay.MessageRequest) (relay.MessageResponse, error)

// enableSwarmMessages подключает роутер к relay-хабу после initialize.
func (m *managedProvider) enableSwarmMessages() {
	if m.hub == nil {
		return
	}
	if m.deliverMessage == nil {
		m.deliverMessage = m.deliverViaHub
	}
	m.hub.SetMessageRouter(m.swarmMessageRouter())
}

// swarmMessageRouter возвращает роутер: отправитель уже авторизован хабом,
// здесь проверяется адресат (тот же рой, живой актор) и выполняется доставка.
func (m *managedProvider) swarmMessageRouter() relay.MessageRouter {
	return func(fromUID string, req relay.MessageRequest) relay.MessageResponse {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		from, err := m.server.store.GetManagedActor(ctx, fromUID)
		if err != nil || from.Phase == "deleted" {
			return relay.MessageResponse{OK: false, Verdict: "sender is not an active actor"}
		}
		if req.From != from.Name {
			return relay.MessageResponse{OK: false, Verdict: "from does not match the sending actor"}
		}
		to, err := m.server.store.LookupManagedActor(ctx, from.SwarmID, req.To)
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
		// урезал бы его молча. Общий потолок остаётся конечным.
		dctx, dcancel := context.WithTimeout(context.Background(), time.Duration(req.WaitSec)*time.Second+15*time.Second)
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
func (m *managedProvider) deliverViaHub(ctx context.Context, toUID string, toGeneration int64, req relay.MessageRequest) (relay.MessageResponse, error) {
	if m.hub == nil {
		return relay.MessageResponse{}, fmt.Errorf("relay hub unavailable")
	}
	frame, err := json.Marshal(req)
	if err != nil {
		return relay.MessageResponse{}, err
	}
	conn, err := m.hub.DialMessage(ctx, toUID, toGeneration, frame)
	if err != nil {
		return relay.MessageResponse{}, err
	}
	defer conn.Close()
	return relay.ReadMessageResponse(conn)
}
