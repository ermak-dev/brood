// FEATURE: Приём сообщений роя гостем (эпик #113, слой 3).
// SPEC: гость принимает канал brood-swarm-msg от сервера, читает запрос из
// ExtraData, передаёт его обработчику и пишет его вердикт в канал; терминал
// не затронут; без обработчика канал отвергается.
package relay

import (
	"testing"
	"time"
)

// Гость-обработчик: доставка в сессию Claude Code делает супервизор,
// для релея это просто функция запрос -> вердикт.
func TestGuestServesSwarmMessageChannel(t *testing.T) {
	f := setup(t)
	// Сервер пусть просто занимает соединение: канал откроет сам тест.
	f.h.SetMessageRouter(func(string, MessageRequest) MessageResponse {
		return MessageResponse{OK: true, Verdict: "server ok"}
	})
	// Гость: RunClient с обработчиком сообщений.
	guestGot := make(chan MessageRequest, 2)
	f.cfg.MessageHandler = func(req MessageRequest) MessageResponse {
		guestGot <- req
		return MessageResponse{OK: true, Verdict: "delivered to session"}
	}
	start(t, f, f.cfg)
	// Серверная сторона инициирует канал, как это делает роутер доставки.
	frame, err := encodeMessageFrame(MessageRequest{To: "me", Body: "hi", From: "peer"})
	if err != nil {
		t.Fatal(err)
	}
	mconn, err := f.h.DialMessage(t.Context(), "actor-uid", 3, frame)
	if err != nil {
		t.Fatal(err)
	}
	defer mconn.Close()
	mconn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := ReadMessageResponse(mconn)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Verdict != "delivered to session" {
		t.Fatalf("verdict: %#v", resp)
	}
	select {
	case req := <-guestGot:
		if req.Body != "hi" || req.From != "peer" {
			t.Fatalf("guest request: %#v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guest handler was not called")
	}
}
