package server

// FEATURE: Outcome of a Telegram send and the bot it goes through (#282).
// SPEC: a send Telegram accepted is sent with its message number; a send
// whose outcome is unknown is unconfirmed, however old the notification; a
// send that certainly did not happen (no connection, a context done before
// it, 429) is queued again whatever the number of attempts, and 429 waits at
// least its retry_after; only a notification older than a day
// (telegramSendMaxAge) is then failed, and a creation time that does not
// parse counts as older. Any other refusal fails at once. An attempt that
// sends nothing because a store read failed is queued again until the
// attempt limit, then failed, unless the read failed because the server
// stops: then it is queued again at any attempt. One whose own bot or event
// swarm is gone fails at once. A chat linked through the user's own bot gets
// the notification through that bot, never through the installation bot.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

func TestSendOutcomeFollowsWhatTelegramKnows(t *testing.T) {
	client, err := telegram.NewClient(telegramProbeToken, telegram.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Контекст отменён до отправки: запрос заведомо не ушёл.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, notSent := client.SendMessage(ctx, 1, "hello", nil)
	if !errors.Is(notSent, telegram.ErrNotSent) {
		t.Fatalf("send with a cancelled context: %v", notSent)
	}
	uncertain := fmt.Errorf("%w: sendMessage: connection reset", telegram.ErrUncertain)
	limited := &telegram.Error{Method: "sendMessage", Code: http.StatusTooManyRequests, RetryAfter: 3 * time.Second}
	blocked := &telegram.Error{Method: "sendMessage", Code: http.StatusForbidden}
	invalid := fmt.Errorf("%w: message text exceeds 4096 characters", telegram.ErrInvalid)
	for _, tc := range []struct {
		name    string
		message telegram.Message
		err     error
		stale   bool
		want    store.TelegramOutcome
		wait    time.Duration
	}{
		{"sent", telegram.Message{MessageID: 42}, nil, false, store.TelegramOutcome{State: "sent", MessageID: 42}, 0},
		{"sent when stale", telegram.Message{MessageID: 42}, nil, true, store.TelegramOutcome{State: "sent", MessageID: 42}, 0},
		{"uncertain", telegram.Message{}, uncertain, false, store.TelegramOutcome{State: "unconfirmed"}, 0},
		{"uncertain when stale", telegram.Message{}, uncertain, true, store.TelegramOutcome{State: "unconfirmed"}, 0},
		{"not sent", telegram.Message{}, notSent, false, store.TelegramOutcome{State: "failed", Requeue: true}, 0},
		{"not sent when stale", telegram.Message{}, notSent, true, store.TelegramOutcome{State: "failed"}, 0},
		{"rate limited", telegram.Message{}, limited, false, store.TelegramOutcome{State: "failed", Requeue: true}, 3 * time.Second},
		{"rate limited when stale", telegram.Message{}, limited, true, store.TelegramOutcome{State: "failed"}, 3 * time.Second},
		{"refused", telegram.Message{}, blocked, false, store.TelegramOutcome{State: "failed"}, 0},
		{"invalid", telegram.Message{}, invalid, false, store.TelegramOutcome{State: "failed"}, 0},
	} {
		got, wait := telegramSendOutcome(tc.message, tc.err, tc.stale)
		if got != tc.want || wait != tc.wait {
			t.Errorf("%s: %+v after %s, want %+v after %s", tc.name, got, wait, tc.want, tc.wait)
		}
	}
}

// Уведомление, которое заведомо не ушло, ждёт Telegram без предела попыток;
// списать его можно только старше суток: такой очереди сбой не накапливает.
// Время создания, которое не прочесть, — старше: строку записали мимо
// хранилища.
func TestNotificationIsStaleOnlyPastTheMaximumAge(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	for _, tc := range []struct {
		name    string
		created string
		stale   bool
	}{
		{"just queued", at(0), false},
		{"an hour old", at(time.Hour), false},
		{"exactly the maximum age", at(telegramSendMaxAge), false},
		{"past the maximum age", at(telegramSendMaxAge + time.Second), true},
		{"queued by a clock ahead", at(-time.Hour), false},
		{"SQLite timestamp", now.Add(-telegramSendMaxAge - time.Minute).Format("2006-01-02T15:04:05.000Z"), true},
		{"unreadable", "yesterday", true},
		{"empty", "", true},
	} {
		if got := telegramStale(tc.created, now); got != tc.stale {
			t.Errorf("%s (%q): stale %t, want %t", tc.name, tc.created, got, tc.stale)
		}
	}
}

// Попытка, которая не дошла до отправки: база не прочла токен своего бота
// или объекты события — failed и снова в очередь, пока попыток меньше
// предела, на пределе — failed; свой бот снят или рой события исчез —
// failed сразу, на любой попытке: повтор ничего не изменит. Чтение,
// которое оборвала остановка сервера, — не сбой уведомления: оно снова в
// очереди и за пределом попыток, которые могли накопить отправки, заведомо
// не ушедшие за долгий сбой сети.
func TestAttemptWithoutASendFollowsWhyNothingWasSent(t *testing.T) {
	live := context.Background()
	stopping, stop := context.WithCancel(live)
	stop()
	// Закрытая база отказывает каждому чтению ошибкой, а не ErrNotFound.
	broken, _ := openSenderStore(t)
	if err := broken.Close(); err != nil {
		t.Fatal(err)
	}
	empty, _ := openSenderStore(t)
	const limit = 3
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		st       *store.Store
		ownBot   bool
		attempts int64
		want     store.TelegramOutcome
	}{
		{"own bot token unread below the limit", live, broken, true, limit - 1, store.TelegramOutcome{State: "failed", Requeue: true}},
		{"own bot token unread at the limit", live, broken, true, limit, store.TelegramOutcome{State: "failed"}},
		{"event unread below the limit", live, broken, false, limit - 1, store.TelegramOutcome{State: "failed", Requeue: true}},
		{"event unread at the limit", live, broken, false, limit, store.TelegramOutcome{State: "failed"}},
		{"own bot token read cut by the shutdown past the limit", stopping, empty, true, 10 * limit, store.TelegramOutcome{State: "failed", Requeue: true}},
		{"event read cut by the shutdown past the limit", stopping, empty, false, 10 * limit, store.TelegramOutcome{State: "failed", Requeue: true}},
		{"own bot gone", live, empty, true, 1, store.TelegramOutcome{State: "failed"}},
		{"swarm of the event gone", live, empty, false, 1, store.TelegramOutcome{State: "failed"}},
	} {
		sender := newTelegramSender(tc.st, Config{telegramBotToken: installBotToken}, telegramOptions{sendAttempts: limit})
		bots := telegramBotClients{config: sender.client}
		n := store.TelegramNotification{ID: 1, UserID: "alice", Kind: "pass_finished", Event: store.TelegramEvent{Swarm: "s1", Sequence: 1, Execution: "stopped"},
			Attempts: tc.attempts, ChatID: mirrorChat, OwnBot: tc.ownBot}
		got, wait := sender.attempt(tc.ctx, &bots, n)
		bots.close()
		if got != tc.want || wait != 0 {
			t.Errorf("%s: %+v after %s, want %+v at once", tc.name, got, wait, tc.want)
		}
	}
}

// openSenderStore — пустое хранилище для отправителя без сервера и путь его
// базы.
func openSenderStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := t.TempDir() + "/brood.db"
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

// execAt выполняет statement в базе path своим соединением, мимо
// хранилища; годна для чужой горутины: не зовёт t.Fatal.
func execAt(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Error(err)
		return
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA busy_timeout=10000`); err != nil {
		t.Error(err)
		return
	}
	if _, err := db.Exec(statement); err != nil {
		t.Errorf("%s: %v", statement, err)
	}
}

// settleFails — сбой записи исхода: база отказывает каждой записи, которая
// закрывает попытку.
const settleFails = `CREATE TRIGGER telegram_settle_fails BEFORE UPDATE OF settled_attempts ON telegram_outbox
	WHEN NEW.settled_attempts > OLD.settled_attempts BEGIN SELECT RAISE(ABORT, 'disk full'); END`

// Запись исхода, которую повтор не исправит, не повторяется: попытки в
// полёте нет или исход неверен. Отказ базы повторяется после паузы, пока
// сервер жив, а остановка, заставшая паузу, оставляет одну последнюю запись:
// удалась она — исход записан, нет — работник уходит с её отказом.
func TestSettleRetriesOnlyWhatARetryCanFix(t *testing.T) {
	ctx := context.Background()
	st, path := openSenderStore(t)
	pauses := 0
	var onPause func(failures int)
	sender := newTelegramSender(st, Config{}, telegramOptions{backoff: func(failures int) time.Duration {
		pauses++
		onPause(failures)
		return time.Millisecond
	}})
	sent := store.TelegramOutcome{State: "sent", MessageID: 7}
	// Пауза здесь — уже ошибка: отмена кончает повтор, и проверка ниже падает.
	refusals, refused := context.WithCancel(ctx)
	defer refused()
	onPause = func(int) { refused() }
	if err := sender.settle(refusals, 1, sent); !errors.Is(err, store.ErrTelegramNoAttempt) {
		t.Fatalf("settle without an attempt in flight: %v", err)
	}
	if err := sender.settle(refusals, 1, store.TelegramOutcome{State: "pending"}); !errors.Is(err, store.ErrTelegramOutcomeInvalid) {
		t.Fatalf("settle of an invalid outcome: %v", err)
	}
	if pauses != 0 {
		t.Fatalf("%d pauses before refusals a retry cannot fix", pauses)
	}
	alice := createTelegramUser(t, ctx, st, "alice")
	inFlight := func() int64 {
		id := stoppedAt(t, path, alice, 1)
		execAt(t, path, `UPDATE telegram_outbox SET attempts = 1 WHERE id = `+strconv.FormatInt(id, 10))
		return id
	}
	state := func(id int64) string {
		var got string
		if err := sqlRow(path, `SELECT state || ':' || attempts || '/' || settled_attempts || ':' || coalesce(telegram_message_id, '') FROM telegram_outbox WHERE id = ?`, id, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	execAt(t, path, settleFails)
	for _, tc := range []struct {
		name string
		// pause — на какой паузе останавливается сервер; recover — база
		// снова пишет к этому моменту.
		pause   int
		recover bool
		want    string
	}{
		{"the database recovers by the shutdown", 2, true, "sent:1/1:7"},
		{"the database still fails at the shutdown", 3, false, "pending:1/0:"},
	} {
		id := inFlight()
		pauses = 0
		live, stop := context.WithCancel(ctx)
		onPause = func(failures int) {
			if failures == tc.pause {
				if tc.recover {
					execAt(t, path, `DROP TRIGGER telegram_settle_fails`)
				}
				stop()
			}
		}
		err := sender.settle(live, id, sent)
		stop()
		if (err == nil) != tc.recover || errors.Is(err, store.ErrTelegramNoAttempt) || pauses != tc.pause || state(id) != tc.want {
			t.Errorf("%s: %v after %d pauses, outbox %s; want %s after pause %d", tc.name, err, pauses, state(id), tc.want, tc.pause)
		}
		if tc.recover {
			execAt(t, path, settleFails)
		}
	}
}

// Контекст отправки: остановка, заставшая отправку до начала, её не
// начинает; отправку в полёте остановка обрывает не сразу, а по истечении
// grace.
func TestSendContextGivesAnInFlightSendTheGrace(t *testing.T) {
	before, stop := context.WithCancel(context.Background())
	stop()
	send, release := telegramSendContext(before, time.Hour)
	if send.Err() == nil {
		t.Fatal("a send the shutdown caught before it began may start")
	}
	release()

	server, stop := context.WithCancel(context.Background())
	send, release = telegramSendContext(server, time.Hour)
	stop()
	select {
	case <-send.Done():
		t.Fatal("the shutdown cut a send in flight at once")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if send.Err() == nil {
		t.Fatal("a finished send keeps its context")
	}

	server, stop = context.WithCancel(context.Background())
	send, release = telegramSendContext(server, 10*time.Millisecond)
	defer release()
	stop()
	select {
	case <-send.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("the grace of a send in flight never ends")
	}
}

// sqlRow читает одно значение из базы path своим соединением.
func sqlRow(path, query string, arg any, dest any) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.QueryRow(query, arg).Scan(dest)
}

func TestSenderUsesTheUsersOwnBot(t *testing.T) {
	var bob string
	h := startTelegramHarness(t, newTestBotAPI(t), "", installBotToken, func(ctx context.Context, st *store.Store) {
		seedMirror(t, ctx, st)
		bob = createTelegramUser(t, ctx, st, "bob")
		if err := st.AddMember(ctx, "p", bob); err != nil {
			t.Fatal(err)
		}
		if err := st.SetSwarmMember(ctx, "s1", bob, "viewer"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetTelegramBot(ctx, bob, aliceBotToken, "Alice_Bot", ""); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if _, err := st.IssueTelegramLinkCode(ctx, bob, "mirror-link-code-own", now, now.Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ConfirmTelegramLink(ctx, store.TelegramLinkConfirmation{BotUsername: "alice_bot", Code: "mirror-link-code-own", ChatID: 601, TelegramUserID: 601, Now: now}); err != nil {
			t.Fatal(err)
		}
	})
	id := stopped(t, h, bob, 1)
	h.settledAs(id, "sent:1/1")
	if texts := h.api.sentTo(aliceBotToken, 601); len(texts) != 1 || !strings.Contains(texts[0], "pass 1 was stopped") {
		t.Fatalf("own bot messages: %q", texts)
	}
	if n := len(h.api.sent(installBotToken)); n != 0 {
		t.Fatalf("%d messages went through the installation bot", n)
	}
}
