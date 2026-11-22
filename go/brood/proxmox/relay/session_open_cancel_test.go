package relay

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// BUG: Гость, не отвечающий на channel-open, может навсегда удержать owner gate.
// HYPOTHESIS: OpenChannel не учитывает отмену context без закрытия SSH peer.
// SOLUTION: Ограниченный opening context закрывает зависший peer до освобождения gate.
// RESULT: FIXED — отмена реального SSH channel-open освобождает gate без поздней отправки.
// STATUS: FIXED
func TestSessionCancelledOpeningReleasesGuard(t *testing.T) {
	f := setup(t)
	conn, channels := rawSSH(t, f, testToken)
	if conn == nil {
		t.Fatal("guest authentication failed")
	}
	await(t, func() bool { return f.h.Connected("actor-uid", 3) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	ctx = WithSessionOpenGuard(ctx, func(context.Context) (func(), error) { return func() { close(released) }, nil })
	done := make(chan error, 1)
	go func() {
		_, err := f.h.CallSession(ctx, "actor-uid", 3, SessionRequest{Operation: "restart", Caller: "actor", Arguments: json.RawMessage(`{}`)})
		done <- err
	}()
	var pending ssh.NewChannel
	select {
	case pending = <-channels:
	case <-time.After(time.Second):
		t.Fatal("session channel did not reach guest")
	}
	// Не принимаем и не отклоняем channel: подтверждение открытия не придёт.
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session call ignored cancellation")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("cancelled channel opening retained owner gate indefinitely")
	}
	// Disconnect приходит другой стороне асинхронно. Уже полученный до
	// отмены запрос остаётся uncertain, а не становится доказанно отменённым.
	disconnected := make(chan error, 1)
	go func() { disconnected <- conn.Wait() }()
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("cancelled opening left the SSH peer connected")
	}
	if ch, _, err := pending.Accept(); err == nil {
		ch.Close()
		t.Fatal("guest accepted a late opening after the gate was released")
	}
}
