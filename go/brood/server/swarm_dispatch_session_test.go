package server

// FEATURE: Инструкция шага ждёт готовности сессии Claude (#244).
// SPEC: workspace_attest перезапускает сессию Claude актора, и новая сессия
// регистрирует inbox не сразу. Гость, заведомо не отправивший сообщение,
// отвечает notSent; диспетчер повторяет ту же попытку с паузами не дольше
// stepSessionWait, перед каждой отправкой заново проверяя шаг, затем —
// refused с причиной. Неизвестный исход по-прежнему unconfirmed без
// повтора. Остановка роя, перевязка роли и выключение сервера во время
// ожидания следующей попытки не пускают. send и ответ на запрос (#210)
// показывают отказ гостя, как прежде.
// IMPLEMENTATION: swarm_dispatch.go (deliver), command_send.go
// (deliverToActor); время диспетчера — подделка часов и паузы.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

// sessionNotReady — ответ гостя, чья сессия Claude ещё не слушает inbox.
var sessionNotReady = relay.MessageResponse{Verdict: "no unique live Claude session for this project", NotSent: true}

// stepClock — часы диспетчера в тесте: пауза сдвигает время, а не ждёт.
// onPause вызывается на каждой паузе с её номером (с 1) до сдвига.
type stepClock struct {
	mu      sync.Mutex
	now     time.Time
	pauses  []time.Duration
	onPause func(n int)
}

func useStepClock(d *dispatchServer) *stepClock {
	c := &stepClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	d.srv.steps.now = c.Now
	d.srv.steps.pause = c.Pause
	return c
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) Pause(ctx context.Context, d time.Duration) bool {
	c.mu.Lock()
	c.pauses = append(c.pauses, d)
	n, hook := len(c.pauses), c.onPause
	c.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err() == nil
}

func (c *stepClock) taken() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.pauses...)
}

// TestStepDeliveryWaitsForTheActorSession — гость N раз отвечает «не
// отправлено», затем доставляет: та же попытка повторяется с паузами, шаг
// delivering перед каждой отправкой, итог — inflight, и сессия получила
// ровно одно сообщение.
func TestStepDeliveryWaitsForTheActorSession(t *testing.T) {
	const refusals = 3
	d := newDispatchServer(t)
	clock := useStepClock(d)
	received := 0
	d.deliver = func(deliveredMessage) (relay.MessageResponse, error) {
		if _, messages := d.calls(); len(messages) <= refusals {
			return sessionNotReady, nil
		}
		received++
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	d.start("deliver")
	attests, messages := d.calls()
	step := d.last()
	if len(attests) != 1 || len(messages) != refusals+1 || received != 1 || step.State != "inflight" || step.Detail != "" {
		t.Fatalf("delivery after %d refusals: %d attestations, %d messages, %d received, step %+v", refusals, len(attests), len(messages), received, step)
	}
	for _, m := range messages {
		if m.Request != messages[0].Request || m.UID != d.worker.UID || m.Generation != d.worker.Incarnation || m.Step.State != "delivering" || m.Step.Request != step.RequestID {
			t.Fatalf("retry is not the same attempt of a delivering step: %+v, first %+v", m, messages[0])
		}
	}
	pauses := clock.taken()
	if len(pauses) != refusals {
		t.Fatalf("pauses: %v", pauses)
	}
	for _, p := range pauses {
		if p != stepSessionPause {
			t.Fatalf("pause %v, want %v", p, stepSessionPause)
		}
	}
	if got := d.complete(d.worker, step, "success"); !got.OK {
		t.Fatalf("completion after a retried delivery: %+v", got)
	}
}

// TestStepDeliverySessionNeverReady — сессия так и не готова: попытки идут
// весь предел stepSessionWait и не дольше, затем refused с причиной и
// ответом гостя.
func TestStepDeliverySessionNeverReady(t *testing.T) {
	d := newDispatchServer(t)
	clock := useStepClock(d)
	d.deliver = func(deliveredMessage) (relay.MessageResponse, error) { return sessionNotReady, nil }
	began := clock.Now()
	d.start("deliver")
	_, messages := d.calls()
	step := d.last()
	want := int(stepSessionWait / stepSessionPause)
	if len(messages) != want || len(clock.taken()) != want-1 {
		t.Fatalf("attempts: %d messages, %d pauses, want %d messages", len(messages), len(clock.taken()), want)
	}
	if waited := clock.Now().Sub(began); waited >= stepSessionWait {
		t.Fatalf("waited %v, limit %v", waited, stepSessionWait)
	}
	if step.State != "refused" || step.FinishedAt == nil || !strings.Contains(step.Detail, "instruction not sent: the actor session did not become ready") || !strings.Contains(step.Detail, "no unique live Claude session") {
		t.Fatalf("session never ready: %+v", step)
	}
}

// TestStepDeliveryUnknownRefusalIsNotRetried — отказ без notSent (прежний
// гость с тем же текстом, запись без квитанции): исход неизвестен —
// unconfirmed, одно сообщение, без паузы и повтора.
func TestStepDeliveryUnknownRefusalIsNotRetried(t *testing.T) {
	for name, reply := range map[string]relay.MessageResponse{
		"old guest":         {Verdict: "no unique live Claude session for this project"},
		"unconfirmed write": {Verdict: "submitted; delivery unconfirmed"},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDispatchServer(t)
			clock := useStepClock(d)
			d.deliver = func(deliveredMessage) (relay.MessageResponse, error) { return reply, nil }
			d.start("deliver")
			_, messages := d.calls()
			step := d.last()
			if len(messages) != 1 || len(clock.taken()) != 0 || step.State != "unconfirmed" || !strings.Contains(step.Detail, "outcome is unknown") || !strings.Contains(step.Detail, reply.Verdict) {
				t.Fatalf("unknown refusal: %d messages, %d pauses, step %+v", len(messages), len(clock.taken()), step)
			}
		})
	}
}

// TestStepChangedWhileWaitingForTheSession — пока диспетчер ждёт сессию,
// рой остановили, роль перевязали или сервер выключается: следующая попытка
// не уходит. Остановленный шаг delivering — unconfirmed, как при всякой
// остановке во время доставки; перевязка и выключение — refused.
func TestStepChangedWhileWaitingForTheSession(t *testing.T) {
	for _, tc := range []struct {
		name, state, detail string
		change              func(*dispatchServer, context.CancelFunc)
	}{
		{"swarm stopped", "unconfirmed", "swarm stopped", func(d *dispatchServer, _ context.CancelFunc) { d.stop() }},
		{"role rebound", "refused", "instruction not sent", func(d *dispatchServer, _ context.CancelFunc) { d.bind("build", d.master.UID) }},
		{"server stopped", "refused", "the server stopped before the send", func(_ *dispatchServer, cancel context.CancelFunc) { cancel() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDispatchServer(t)
			clock := useStepClock(d)
			d.startPending("deliver")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock.onPause = func(int) { tc.change(d, cancel) }
			d.deliver = func(deliveredMessage) (relay.MessageResponse, error) { return sessionNotReady, nil }
			d.srv.steps.dispatch(ctx, d.swarm)
			_, messages := d.calls()
			step := d.last()
			if len(messages) != 1 || len(clock.taken()) != 1 || step.State != tc.state || !strings.Contains(step.Detail, tc.detail) {
				t.Fatalf("%s while waiting: %d messages, %d pauses, step %+v", tc.name, len(messages), len(clock.taken()), step)
			}
		})
	}
}

// TestStepNotSentAfterTheStepChanged — гость ответил «не отправлено», а роль
// за время вызова перевязали: инструкция заведомо не ушла — refused, а не
// unconfirmed, и без повтора.
func TestStepNotSentAfterTheStepChanged(t *testing.T) {
	d := newDispatchServer(t)
	clock := useStepClock(d)
	d.deliver = func(deliveredMessage) (relay.MessageResponse, error) {
		d.bind("build", d.master.UID)
		return sessionNotReady, nil
	}
	d.start("deliver")
	_, messages := d.calls()
	step := d.last()
	if len(messages) != 1 || len(clock.taken()) != 0 || step.State != "refused" || !strings.Contains(step.Detail, "instruction not sent") {
		t.Fatalf("not sent after the step changed: %d messages, %d pauses, step %+v", len(messages), len(clock.taken()), step)
	}
}

// TestOperatorSendShowsNotSentVerdict — send оператора не повторяет отказ
// гостя: ответ, как прежде, — вердикт гостя с исходом unconfirmed, одно
// обращение к гостю.
func TestOperatorSendShowsNotSentVerdict(t *testing.T) {
	ts, alice, master, _ := masterSetup(t)
	calls := 0
	ts.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
		calls++
		return sessionNotReady, nil
	}
	r, b, raw := ts.do(alice, "POST", "/api/commands/send", ts.csrf(alice), map[string]any{"swarm_id": master.SwarmID, "arguments": map[string]any{"to": "master", "message": "hello"}})
	if r.StatusCode != http.StatusOK || calls != 1 || b["text"] != sessionNotReady.Verdict || b["outcome"] != "unconfirmed" {
		t.Fatalf("send to a session that is not ready: %d %s, %d calls", r.StatusCode, raw, calls)
	}
}
