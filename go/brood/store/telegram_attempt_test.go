package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// FEATURE: Telegram sender attempts on the outbox (#282, epic #278).
// SPEC: the sender takes the queue of one user in id order, one attempt at
// a time: an attempt is recorded before the send and the head with an
// attempt in flight gets no second one. The outcome settles the attempt:
// sent with the Telegram message number, failed, unconfirmed, or failed and
// queued again when the message certainly was not sent, so the same
// notification heads the queue again. A notification nobody may receive any
// more (no live link, no subscription to its kind, no right to read its
// swarm, no bot to send it), or whose event does not decode, is retired
// without a send: a synthetic attempt settled failed. After startup recovery
// an interrupted attempt is unconfirmed and the sender goes on with the next
// notification, never the same one. Retention deletes settled notifications older than its cutoff,
// never a queued one or an attempt in flight. The sender finds the users
// with a queue.
// IMPLEMENTATION: BeginTelegramAttempt, SettleTelegramAttempt,
// PruneTelegramOutbox and TelegramOutboxUsers (telegram_outbox.go).
// STATUS: DONE

// queuedStore — mirrorStore после запуска approve и остановки: у каждого
// читателя очередь начала прохода, запроса узла и конца прохода.
func queuedStore(t *testing.T) *Store {
	t.Helper()
	s := mirrorStore(t)
	mustStart(t, s, "approve")
	_, err := s.StopSwarm(context.Background(), "one", "u", allow)
	mustNil(t, err)
	return s
}

func begin(t *testing.T, s *Store, user string, installation bool) TelegramNotification {
	t.Helper()
	n, err := s.BeginTelegramAttempt(context.Background(), user, installation)
	if err != nil {
		t.Fatalf("begin attempt for %s: %v", user, err)
	}
	return n
}

func settle(t *testing.T, s *Store, id int64, outcome TelegramOutcome) {
	t.Helper()
	mustNil(t, s.SettleTelegramAttempt(context.Background(), id, outcome))
}

func TestSenderTakesTheQueueInOrderOneAttemptAtATime(t *testing.T) {
	ctx := context.Background()
	s := queuedStore(t)
	first := begin(t, s, "u", true)
	if first.Kind != "pass_started" || first.UserID != "u" || first.Ref != "swarm/one/pass/1" || first.Attempts != 1 || first.ChatID != 11 || first.OwnBot ||
		!reflect.DeepEqual(first.Event, TelegramEvent{Swarm: "one", Sequence: 1, Node: "ask", Execution: "waiting"}) {
		t.Fatalf("first attempt = %+v", first)
	}
	if got := outboxState(t, s, first.ID); got != "pending:1/0" {
		t.Fatalf("begun attempt = %s, want in flight", got)
	}
	// Голова с попыткой в полёте второй попытки не получает, и следующее
	// уведомление её не обгоняет.
	if _, err := s.BeginTelegramAttempt(ctx, "u", true); !errors.Is(err, ErrTelegramInFlight) {
		t.Fatalf("second attempt while one is in flight: %v", err)
	}
	if got := outboxState(t, s, first.ID); got != "pending:1/0" {
		t.Fatalf("refused second attempt changed the head: %s", got)
	}
	settle(t, s, first.ID, TelegramOutcome{State: "sent", MessageID: 501})
	var message int64
	mustNil(t, s.db.QueryRow(`SELECT telegram_message_id FROM telegram_outbox WHERE id = ?`, first.ID).Scan(&message))
	if got := outboxState(t, s, first.ID); got != "sent:1/1" || message != 501 {
		t.Fatalf("sent notification = %s, message %d", got, message)
	}
	// Сообщение заведомо не ушло: та же строка снова голова очереди.
	second := begin(t, s, "u", true)
	if second.Kind != "request_opened" || second.Attempts != 1 {
		t.Fatalf("second notification = %+v", second)
	}
	settle(t, s, second.ID, TelegramOutcome{State: "failed", Requeue: true})
	if got := outboxState(t, s, second.ID); got != "pending:1/1" {
		t.Fatalf("requeued notification = %s", got)
	}
	again := begin(t, s, "u", true)
	if again.ID != second.ID || again.Attempts != 2 {
		t.Fatalf("after a requeue the sender took %+v, want notification %d again", again, second.ID)
	}
	settle(t, s, again.ID, TelegramOutcome{State: "unconfirmed"})
	third := begin(t, s, "u", true)
	if third.Kind != "pass_finished" || !reflect.DeepEqual(third.Event, TelegramEvent{Swarm: "one", Sequence: 1, Execution: "stopped"}) {
		t.Fatalf("third notification = %+v", third)
	}
	settle(t, s, third.ID, TelegramOutcome{State: "failed"})
	for id, want := range map[int64]string{second.ID: "unconfirmed:2/2", third.ID: "failed:1/1"} {
		if got := outboxState(t, s, id); got != want {
			t.Fatalf("notification %d = %s, want %s", id, got, want)
		}
	}
	if _, err := s.BeginTelegramAttempt(ctx, "u", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attempt on an empty queue: %v", err)
	}
	// Очередь другого пользователя не тронута.
	if n := begin(t, s, "v", true); n.Kind != "pass_started" || n.ChatID != 12 {
		t.Fatalf("queue of v = %+v", n)
	}
}

func TestSettleNeedsAnAttemptInFlightAndAKnownOutcome(t *testing.T) {
	ctx := context.Background()
	s := queuedStore(t)
	head := begin(t, s, "u", true)
	for _, outcome := range []TelegramOutcome{
		{State: "pending"},
		{State: "sent"},
		{State: "sent", MessageID: 7, Requeue: true},
		{State: "unconfirmed", Requeue: true},
		{State: "failed", MessageID: 7},
		{State: "delivered"},
	} {
		if err := s.SettleTelegramAttempt(ctx, head.ID, outcome); !errors.Is(err, ErrTelegramOutcomeInvalid) {
			t.Fatalf("settle %+v: %v", outcome, err)
		}
	}
	settle(t, s, head.ID, TelegramOutcome{State: "sent", MessageID: 7})
	// Записанный исход второй раз не пишется, а уведомление без попытки
	// исхода не получает.
	if err := s.SettleTelegramAttempt(ctx, head.ID, TelegramOutcome{State: "failed"}); !errors.Is(err, ErrTelegramNoAttempt) {
		t.Fatalf("settle a settled notification: %v", err)
	}
	var queuedID int64
	mustNil(t, s.db.QueryRow(`SELECT id FROM telegram_outbox WHERE user_id = 'v' ORDER BY id LIMIT 1`).Scan(&queuedID))
	if err := s.SettleTelegramAttempt(ctx, queuedID, TelegramOutcome{State: "sent", MessageID: 8}); !errors.Is(err, ErrTelegramNoAttempt) {
		t.Fatalf("settle a queued notification without an attempt: %v", err)
	}
	if got := outboxState(t, s, queuedID); got != "pending:0/0" {
		t.Fatalf("refused settle changed the notification: %s", got)
	}
}

func TestSenderRetiresWhatNobodyMayReceive(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, user   string
		installation bool
		change       func(t *testing.T, s *Store)
	}{
		{"unlinked", "root", true, func(t *testing.T, s *Store) {
			_, err := s.UnlinkTelegram(ctx, "root")
			mustNil(t, err)
		}},
		{"unsubscribed", "v", true, func(t *testing.T, s *Store) {
			_, err := s.SetTelegramEvents(ctx, "v", []string{"request_opened"})
			mustNil(t, err)
		}},
		{"session revoked", "v", true, func(t *testing.T, s *Store) {
			_, err := s.DeleteSessionsOfUser(ctx, "v")
			mustNil(t, err)
		}},
		{"disabled", "v", true, func(t *testing.T, s *Store) {
			_, err := s.db.Exec(`UPDATE users SET disabled = 1 WHERE id = 'v'`)
			mustNil(t, err)
		}},
		{"swarm role removed", "v", true, func(t *testing.T, s *Store) {
			mustNil(t, s.RemoveSwarmMember(ctx, "one", "v"))
		}},
		{"project membership removed", "v", true, func(t *testing.T, s *Store) {
			mustNil(t, s.RemoveMember(ctx, "one", "v"))
		}},
		{"administrator role removed", "root", true, func(t *testing.T, s *Store) {
			// Последнего администратора хранилище не снимает: сначала
			// назначается другой.
			mustNil(t, s.SetUserAdmin(ctx, "w", true))
			mustNil(t, s.SetUserAdmin(ctx, "root", false))
		}},
		{"swarm deleted", "u", true, func(t *testing.T, s *Store) {
			_, err := s.db.Exec(`UPDATE swarms SET state = 'deleted' WHERE id = 'one'`)
			mustNil(t, err)
		}},
		{"no installation bot", "u", false, func(t *testing.T, s *Store) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := queuedStore(t)
			tc.change(t, s)
			var head int64
			mustNil(t, s.db.QueryRow(`SELECT id FROM telegram_outbox WHERE user_id = ? ORDER BY id LIMIT 1`, tc.user).Scan(&head))
			if _, err := s.BeginTelegramAttempt(ctx, tc.user, tc.installation); !errors.Is(err, ErrTelegramUndeliverable) {
				t.Fatalf("attempt: %v, want the notification retired", err)
			}
			// Синтетическая попытка без отправки: failed с записанным
			// исходом, его удалит срок хранения.
			if got := outboxState(t, s, head); got != "failed:1/1" {
				t.Fatalf("retired notification = %s", got)
			}
		})
	}
	t.Run("unsubscribed from one kind only", func(t *testing.T) {
		s := queuedStore(t)
		_, err := s.SetTelegramEvents(ctx, "v", []string{"request_opened"})
		mustNil(t, err)
		if _, err := s.BeginTelegramAttempt(ctx, "v", true); !errors.Is(err, ErrTelegramUndeliverable) {
			t.Fatalf("attempt on an unsubscribed kind: %v", err)
		}
		if n := begin(t, s, "v", true); n.Kind != "request_opened" {
			t.Fatalf("after retiring, the sender took %+v", n)
		}
	})
	t.Run("own bot needs no installation bot", func(t *testing.T) {
		s := mirrorStore(t)
		_, err := s.UnlinkTelegram(ctx, "u")
		mustNil(t, err)
		_, err = s.SetTelegramBot(ctx, "u", "710000001:own-bot-secret-0123456789abcdefghij", "own_bot", "")
		mustNil(t, err)
		linkTelegram(t, s, "u", "own_bot", 21)
		mustStart(t, s, "approve")
		if n := begin(t, s, "u", false); !n.OwnBot || n.ChatID != 21 || n.Kind != "pass_started" {
			t.Fatalf("own bot attempt = %+v", n)
		}
	})
}

// Голова, чьё событие не прочесть (не JSON или поле другого типа: строку
// записали мимо хранилища или другая версия), не держит очередь: её снимает
// та же синтетическая попытка failed без отправки, и очередь идёт дальше.
func TestUnreadableHeadIsRetiredAndTheQueueGoesOn(t *testing.T) {
	ctx := context.Background()
	for name, payload := range map[string]string{
		"not JSON":        `not json`,
		"field type":      `{"swarm":"one","sequence":"one"}`,
		"event not a map": `["one"]`,
	} {
		t.Run(name, func(t *testing.T) {
			s := mirrorStore(t)
			result, err := s.db.Exec(`INSERT INTO telegram_outbox (user_id, kind, ref, payload, state, attempts, settled_attempts, created_at, updated_at)
				VALUES ('u', 'pass_started', 'swarm/one/pass/1', ?, 'pending', 0, 0, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))`, payload)
			mustNil(t, err)
			unreadable, err := result.LastInsertId()
			mustNil(t, err)
			mustStart(t, s, "approve")
			_, err = s.BeginTelegramAttempt(ctx, "u", true)
			if !errors.Is(err, ErrTelegramUndeliverable) || !strings.Contains(err.Error(), "payload is unreadable") {
				t.Fatalf("attempt on an unreadable head: %v, want it retired as unreadable", err)
			}
			if got := outboxState(t, s, unreadable); got != "failed:1/1" {
				t.Fatalf("unreadable head = %s, want retired", got)
			}
			if n := begin(t, s, "u", true); n.ID == unreadable || n.Kind != "pass_started" || n.Event.Swarm != "one" {
				t.Fatalf("after the unreadable head the sender took %+v", n)
			}
		})
	}
}

func TestRecoveredAttemptIsNotRepeatedByTheNextSender(t *testing.T) {
	ctx := context.Background()
	s := queuedStore(t)
	interrupted := begin(t, s, "u", true)
	// Сервер упал между отметкой попытки и её исходом.
	mustNil(t, s.RecoverTelegramOutbox(ctx))
	if got := outboxState(t, s, interrupted.ID); got != "unconfirmed:1/1" {
		t.Fatalf("recovered attempt = %s", got)
	}
	next := begin(t, s, "u", true)
	if next.ID == interrupted.ID || next.Kind != "request_opened" {
		t.Fatalf("after recovery the sender took %+v, want the next notification", next)
	}
	if got := outboxState(t, s, interrupted.ID); got != "unconfirmed:1/1" {
		t.Fatalf("the recovered attempt was attempted again: %s", got)
	}
}

func TestPruneDeletesOnlySettledNotificationsPastTheCutoff(t *testing.T) {
	ctx := context.Background()
	s := queuedStore(t)
	sent := begin(t, s, "u", true)
	settle(t, s, sent.ID, TelegramOutcome{State: "sent", MessageID: 1})
	unconfirmed := begin(t, s, "u", true)
	settle(t, s, unconfirmed.ID, TelegramOutcome{State: "unconfirmed"})
	inFlight := begin(t, s, "u", true)
	retried := begin(t, s, "v", true)
	settle(t, s, retried.ID, TelegramOutcome{State: "failed", Requeue: true})
	failed := begin(t, s, "root", true)
	settle(t, s, failed.ID, TelegramOutcome{State: "failed"})
	total := len(queued(t, s))
	if n, err := s.PruneTelegramOutbox(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("prune before the notifications settled: %d, %v", n, err)
	}
	n, err := s.PruneTelegramOutbox(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("prune past the cutoff: %d, %v; want the sent, unconfirmed and failed ones", n, err)
	}
	if left := len(queued(t, s)); left != total-3 {
		t.Fatalf("%d notifications left of %d", left, total)
	}
	for id, want := range map[int64]string{inFlight.ID: "pending:1/0", retried.ID: "pending:1/1"} {
		if got := outboxState(t, s, id); got != want {
			t.Fatalf("kept notification %d = %s, want %s", id, got, want)
		}
	}
}

func TestTelegramOutboxUsersAreThoseWithAQueue(t *testing.T) {
	ctx := context.Background()
	s := queuedStore(t)
	users, err := s.TelegramOutboxUsers(ctx)
	mustNil(t, err)
	sameStrings(t, "users with a queue", users, []string{"root", "u", "v"})
	for range 3 {
		n := begin(t, s, "root", true)
		settle(t, s, n.ID, TelegramOutcome{State: "sent", MessageID: n.ID})
	}
	// Попытка в полёте — ещё очередь, и тогда, когда других уведомлений у
	// пользователя нет: у u остаётся одно, и его попытка в полёте.
	for range 2 {
		n := begin(t, s, "u", true)
		settle(t, s, n.ID, TelegramOutcome{State: "sent", MessageID: n.ID})
	}
	last := begin(t, s, "u", true)
	if got := outboxState(t, s, last.ID); got != "pending:1/0" {
		t.Fatalf("last notification of u = %s, want its attempt in flight", got)
	}
	var others int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM telegram_outbox WHERE user_id = 'u' AND state = 'pending' AND id <> ?`, last.ID).Scan(&others))
	if others != 0 {
		t.Fatalf("u still has %d other queued notifications", others)
	}
	users, err = s.TelegramOutboxUsers(ctx)
	mustNil(t, err)
	sameStrings(t, "users with a queue", users, []string{"u", "v"})
}
