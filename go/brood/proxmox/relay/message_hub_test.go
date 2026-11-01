// FEATURE: Канал сообщений роя через гостевой relay (эпик #113, слой 2).
// SPEC: авторизованный гость открывает канал brood-swarm-msg, запрос уходит
// в роутер сервера, вердикт возвращается в тот же канал; без роутера и при
// выходе за границы контракта канал отвергается; терминал не затронут.
package relay

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func sendMessage(t *testing.T, f *fixture, conn ssh.Conn, req MessageRequest) (MessageResponse, error) {
	t.Helper()
	frame, err := encodeMessageFrame(req)
	if err != nil {
		return MessageResponse{}, err
	}
	ch, reqs, err := conn.OpenChannel(messageChannel, frame)
	if err != nil {
		return MessageResponse{}, err
	}
	go ssh.DiscardRequests(reqs)
	defer ch.Close()
	done := make(chan MessageResponse, 1)
	go func() {
		resp, err := readMessageResponse(ch)
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	select {
	case resp := <-done:
		return resp, nil
	case <-time.After(10 * time.Second):
		return MessageResponse{}, errors.New("swarm message verdict timeout")
	}
}

func TestSwarmMessageRoutedThroughHub(t *testing.T) {
	f := setup(t)
	seen := make(chan MessageRequest, 1)
	f.h.SetMessageRouter(func(fromUID string, req MessageRequest) MessageResponse {
		if fromUID != "actor-uid" {
			t.Errorf("sender identity: %q", fromUID)
		}
		seen <- req
		return MessageResponse{OK: true, Verdict: "delivered to " + req.To}
	})
	start(t, f, f.cfg)

	// Гость говорит тем же SSH-соединением, что и терминальный транспорт:
	// клиент relay (RunClient) держит его, здесь открываем канал напрямую.
	conn, _ := rawSSH(t, f, testToken)
	resp, err := sendMessage(t, f, conn, MessageRequest{To: "claude", Body: "hello", From: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Verdict != "delivered to claude" {
		t.Fatalf("verdict: %#v", resp)
	}
	select {
	case req := <-seen:
		if req.Body != "hello" || req.From != "worker" {
			t.Fatalf("routed request: %#v", req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("router was not called")
	}
}

func TestSwarmMessageRejectedWithoutRouterAndOnBadFrames(t *testing.T) {
	f := setup(t)
	start(t, f, f.cfg)
	conn, _ := rawSSH(t, f, testToken)
	// Без роутера канал сообщений закрыт, как и любые прочие.
	if _, _, err := conn.OpenChannel(messageChannel, []byte(`{"to":"a","from":"b","body":"c"}`)); err == nil {
		t.Fatal("message channel accepted without router")
	}
	f.h.SetMessageRouter(func(string, MessageRequest) MessageResponse {
		return MessageResponse{OK: true, Verdict: "ok"}
	})
	// Кадр-мусор и пустые поля отвергаются до роутера.
	if _, _, err := conn.OpenChannel(messageChannel, []byte("not json")); err == nil {
		t.Fatal("garbage frame accepted")
	}
	if _, _, err := conn.OpenChannel(messageChannel, []byte(`{"to":"","from":"","body":""}`)); err == nil {
		t.Fatal("empty frame accepted")
	}
	if _, _, err := conn.OpenChannel(messageChannel, nil); err == nil {
		t.Fatal("empty extra data accepted")
	}
	resp, err := sendMessage(t, f, conn, MessageRequest{To: "a", Body: "b", From: "c"})
	if err != nil || !resp.OK {
		t.Fatalf("valid frame after rejections: %v %#v", err, resp)
	}
}

func TestSwarmMessageFailureVerdictIsText(t *testing.T) {
	f := setup(t)
	f.h.SetMessageRouter(func(string, MessageRequest) MessageResponse {
		return MessageResponse{OK: false, Verdict: "recipient is offline"}
	})
	start(t, f, f.cfg)
	conn, _ := rawSSH(t, f, testToken)
	resp, err := sendMessage(t, f, conn, MessageRequest{To: "ghost", Body: "hi", From: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Verdict != "recipient is offline" {
		t.Fatalf("failure verdict: %#v", resp)
	}
}
