package relay

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// BUG: Проверка session RPC вне транспорта допускает позднее открытие канала.
// HYPOTHESIS: Ограждение должно охватывать открытие SSH, но не ожидание ответа.
// SOLUTION: Локальный context guard вызывается непосредственно перед OpenChannel.
// RESULT: FIXED — отказ guard не достигает гостя; ожидание ответа не удерживает gate.
// STATUS: FIXED
func TestSessionOpenGuardRefusesBeforeGuestEffect(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	var effects atomic.Int32
	cfg.Session = func(context.Context, SessionRequest) SessionResponse {
		effects.Add(1)
		return SessionResponse{OK: true}
	}
	start(t, f, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var checked atomic.Int32
	ctx = WithSessionOpenGuard(ctx, func(context.Context) (func(), error) { checked.Add(1); return nil, errors.New("interrupted") })
	_, err := f.h.CallSession(ctx, "actor-uid", 3, SessionRequest{Operation: "slash", Caller: "actor", Arguments: json.RawMessage(`{}`)})
	if err == nil || checked.Load() != 1 || effects.Load() != 0 {
		t.Fatal("unauthorized channel opened", err, checked.Load(), effects.Load())
	}
}

func TestSessionOpenGuardReleasedBeforeGuestResponse(t *testing.T) {
	f := setup(t)
	cfg := f.cfg
	entered, resume, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cfg.Session = func(ctx context.Context, req SessionRequest) SessionResponse {
		if req.Operation == "tail" {
			return SessionResponse{OK: true, Outcome: "complete", Text: "still connected"}
		}
		close(entered)
		select {
		case <-resume:
		case <-ctx.Done():
		}
		return SessionResponse{OK: true, Outcome: "complete"}
	}
	start(t, f, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer close(resume)
	ctx = WithSessionOpenGuard(ctx, func(context.Context) (func(), error) { return func() { close(released) }, nil })
	done := make(chan error, 1)
	go func() {
		_, err := f.h.CallSession(ctx, "actor-uid", 3, SessionRequest{Operation: "slash", Caller: "actor", Arguments: json.RawMessage(`{}`)})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("guest did not receive channel")
	}
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("guest receipt blocked owner gate")
	}
	cancel()
	<-done
	fast, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	got, err := f.h.CallSession(fast, "actor-uid", 3, SessionRequest{Operation: "tail", Caller: "owner", Arguments: json.RawMessage(`{}`)})
	if err != nil || !got.OK || got.Text != "still connected" {
		t.Fatal("cancelling a result wait disconnected another session", got, err)
	}
}
