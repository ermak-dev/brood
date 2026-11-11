package relay

// FEATURE: Гость принимает сообщения сервера через действующее SSH-соединение.
// SPEC: Отдельный ограниченный обработчик, отказ до вызова на плохом кадре,
// отмена контекста сессии и сохранение терминального канала.
// IMPLEMENTATION: Отдельные bounded slots, обработчик и контекст канала.
// RESULT: IMPLEMENTED — приёмный relay, без исходящего MCP broker.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"brood.dev/go/brood/command"
)

func TestGuestMessageHandler(t *testing.T) {
	f := setup(t)
	seen := make(chan MessageRequest, 1)
	cfg := f.cfg
	cfg.HandleMessage = func(ctx context.Context, req MessageRequest) MessageResponse {
		seen <- req
		return MessageResponse{OK: true, Verdict: "queued"}
	}
	start(t, f, cfg)
	req := MessageRequest{To: "worker", From: "lead", Body: "hello"}
	frame, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := f.h.DialMessage(ctx, "actor-uid", 3, frame)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	resp, err := ReadMessageResponse(c)
	if err != nil || !resp.OK || resp.Verdict != "queued" {
		t.Fatalf("response: %#v %v", resp, err)
	}
	if got := <-seen; got != req {
		t.Fatalf("request: %#v", got)
	}
	if c, err := f.h.DialMessage(ctx, "actor-uid", 3, []byte(`{"to":"worker"}`)); err == nil {
		c.Close()
		t.Fatal("invalid frame accepted")
	}
	terminal := dial(t, f)
	go io.WriteString(terminal, "ok")
	b := make([]byte, 2)
	if _, err := io.ReadFull(terminal, b); err != nil || string(b) != "ok" {
		t.Fatal("terminal broken", err)
	}
}

// TestGuestMessageNotSentCrossesRelay — отказ гостя до записи в сессию
// доезжает до сервера полем notSent через настоящий канал (#244).
func TestGuestMessageNotSentCrossesRelay(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	cfg.HandleMessage = func(context.Context, MessageRequest) MessageResponse {
		return MessageResponse{Verdict: "no unique live Claude session for this project", NotSent: true}
	}
	start(t, f, cfg)
	frame, _ := json.Marshal(MessageRequest{To: "worker", From: "swarm:s1", Body: "Step 1"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := f.h.DialMessage(ctx, "actor-uid", 3, frame)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	resp, err := ReadMessageResponse(c)
	if err != nil || resp.OK || !resp.NotSent || resp.Verdict != "no unique live Claude session for this project" {
		t.Fatalf("response: %#v %v", resp, err)
	}
}

// TestGuestMessageHandlerBudget — обработчик гостя получает предел канала
// command.CallTimeout (#260): без ожидания простоя гость ждёт квитанцию
// доставки, и канал не закрывается раньше, чем helper ответит.
func TestGuestMessageHandlerBudget(t *testing.T) {
	f := setup(t)
	budgets := make(chan time.Duration, 2)
	cfg := f.cfg
	cfg.HandleMessage = func(ctx context.Context, req MessageRequest) MessageResponse {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("guest message handler has no deadline")
		}
		budgets <- time.Until(deadline)
		return MessageResponse{OK: true, Verdict: "delivered"}
	}
	start(t, f, cfg)
	for _, wait := range []int{0, 60} {
		frame, _ := json.Marshal(MessageRequest{To: "worker", From: "lead", Body: "hello", WaitSec: wait})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := f.h.DialMessage(ctx, "actor-uid", 3, frame)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if _, err := ReadMessageResponse(c); err != nil {
			t.Fatal(err)
		}
		c.Close()
		cancel()
		want := command.CallTimeout(wait)
		if got := <-budgets; got > want || got < want-2*time.Second {
			t.Errorf("wait %d: handler budget %v, want %v", wait, got, want)
		}
	}
}

func TestGuestMessageHandlerCanceledOnDisconnect(t *testing.T) {
	f := setup(t)
	entered := make(chan struct{})
	stopped := make(chan struct{})
	cfg := f.cfg
	cfg.HandleMessage = func(ctx context.Context, req MessageRequest) MessageResponse {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return MessageResponse{Verdict: "canceled"}
	}
	cancel := start(t, f, cfg)
	frame, _ := json.Marshal(MessageRequest{To: "a", From: "b", Body: "c"})
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	c, err := f.h.DialMessage(ctx, "actor-uid", 3, frame)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-entered
	cancel()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("message handler outlived SSH session")
	}
}

func TestGuestMessageHandlerCanceledOnChannelClose(t *testing.T) {
	f := setup(t)
	entered, stopped := make(chan struct{}), make(chan struct{})
	cfg := f.cfg
	cfg.HandleMessage = func(ctx context.Context, req MessageRequest) MessageResponse {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return MessageResponse{Verdict: "canceled"}
	}
	start(t, f, cfg)
	frame, _ := json.Marshal(MessageRequest{To: "a", From: "b", Body: "c", WaitSec: 300})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := f.h.DialMessage(ctx, "actor-uid", 3, frame)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	c.Close()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("message handler outlived closed delivery channel")
	}
}

func TestGuestMessageDisabledByDefault(t *testing.T) {
	f := setup(t)
	start(t, f, f.cfg)
	frame, _ := json.Marshal(MessageRequest{To: "a", From: "b", Body: "c"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c, err := f.h.DialMessage(ctx, "actor-uid", 3, frame); err == nil {
		c.Close()
		t.Fatal("accepted message without handler")
	}
}
