package server

// FEATURE: Telegram sender of the outbox (#282, epic #278).
// SPEC: after the domain change commits, a sender per user takes the
// user's queue in order, one send at a time, and sends each notification to
// the linked chat through the bot of the link: the user's own bot or the
// installation bot. A sent notification keeps its Telegram message number.
// A send that certainly did not happen (no connection, 429) is tried again
// after a pause, with no bound on attempts, and keeps the head of the queue;
// only a notification older than a day is then failed. A definite refusal
// fails at once; a send whose outcome is unknown is unconfirmed and never
// sent again by the sender. The queue goes on after each outcome. A
// notification whose recipient lost the link is retired without a send. The
// goal of a started pass and the reason of a failed step are read from the
// step when the message is sent. Startup recovery settles an attempt the
// previous process left in flight as unconfirmed, and it is not sent again.
// An outcome the sender learned survives a failed write: it is written again
// after a pause while the server runs and once more at shutdown, and the
// queue goes on. A shutdown gives a send in flight a grace to end and records
// its outcome before the store closes; a send held past the grace is cut,
// unconfirmed. A text over the message limit goes as one document. Settled
// notifications past retention are deleted.
// IMPLEMENTATION: telegramSender (telegram_sender.go), renderTelegramNotice
// (telegram_notice.go), the store methods of telegram_outbox.go.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/store"
)

// mirrorGraph — оператор одобряет, и проход кончается.
const mirrorGraph = `apiVersion: brood.dev/v1alpha1
name: approve
start: ask
exit: done
nodes:
  ask:
    type: human
    question: Ship build 42?
    options:
      - {label: Ship, outcome: success}
  done: {type: exit}
edges:
  - {from: ask, to: done}
goalGates: [done]
`

// mirrorChat — личный чат alice, привязанный через бот установки.
const mirrorChat = 501

// seedMirror заводит alice с чатом mirrorChat через бот установки, её проект
// p и активный рой s1 «release-train», где она owner; возвращает её id.
func seedMirror(t *testing.T, ctx context.Context, st *store.Store) string {
	t.Helper()
	alice := createTelegramUser(t, ctx, st, "alice")
	if err := st.CreateProject(ctx, store.Project{ID: "p", Name: "proj"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, "p", alice); err != nil {
		t.Fatal(err)
	}
	uid := "native-s1"
	if err := st.CreateSwarm(ctx, store.Swarm{ID: "s1", ProjectID: "p", OwnerID: alice, Name: "release-train", AtespaceName: "s1", AtespaceUID: &uid, State: "active"}); err != nil {
		t.Fatal(err)
	}
	linkInStore(t, ctx, st, alice, mirrorChat)
	return alice
}

// linkInStore привязывает чат пользователя через бот установки хранилищем,
// мимо поллера.
func linkInStore(t *testing.T, ctx context.Context, st *store.Store, user string, chat int64) {
	t.Helper()
	now := time.Now()
	code := "mirror-link-code-" + user
	if _, err := st.IssueTelegramLinkCode(ctx, user, code, now, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfirmTelegramLink(ctx, store.TelegramLinkConfirmation{Code: code, ChatID: chat, TelegramUserID: chat, Now: now}); err != nil {
		t.Fatal(err)
	}
}

// outboxInsert ставит уведомление мимо доменной транзакции, как его
// поставило бы событие, и возвращает его id.
func outboxInsert(t *testing.T, dbPath, user, kind, ref, payload string) int64 {
	t.Helper()
	return outboxInsertCreated(t, dbPath, user, kind, ref, payload, time.Now())
}

// outboxInsertCreated — outboxInsert уведомления, поставленного в момент
// created.
func outboxInsertCreated(t *testing.T, dbPath, user, kind, ref, payload string, created time.Time) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA busy_timeout=10000`); err != nil {
		t.Fatal(err)
	}
	at := created.UTC().Format(time.RFC3339Nano)
	result, err := db.Exec(`INSERT INTO telegram_outbox (user_id, kind, ref, payload, state, attempts, settled_attempts, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', 0, 0, ?, ?)`, user, kind, ref, payload, at, at)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// stopped ставит уведомление об остановке прохода pass роя s1.
func stopped(t *testing.T, h *telegramHarness, user string, pass int) int64 {
	t.Helper()
	return stoppedAt(t, h.dbPath, user, pass)
}

func stoppedAt(t *testing.T, dbPath, user string, pass int) int64 {
	t.Helper()
	n := strconv.Itoa(pass)
	return outboxInsert(t, dbPath, user, "pass_finished", "swarm/s1/pass/"+n, `{"swarm":"s1","sequence":`+n+`,"execution":"stopped"}`)
}

func (h *telegramHarness) outboxState(id int64) string {
	h.t.Helper()
	return h.column(`SELECT state || ':' || attempts || '/' || settled_attempts FROM telegram_outbox WHERE id = ?`, id)
}

func (h *telegramHarness) settledAs(id int64, want string) {
	h.t.Helper()
	telegramEventually(h.t, "notification settles as "+want, func() bool { return h.outboxState(id) == want })
}

// mirrorHarness — сервер с ботом установки и seedMirror в базе.
func mirrorHarness(t *testing.T, tune ...func(*telegramOptions)) (*telegramHarness, string) {
	t.Helper()
	var alice string
	h := startTelegramHarness(t, newTestBotAPI(t), "", installBotToken, func(ctx context.Context, st *store.Store) {
		alice = seedMirror(t, ctx, st)
	}, tune...)
	return h, alice
}

func TestSenderMirrorsTheRequestAndItsAnswerInOrder(t *testing.T) {
	ctx := context.Background()
	h, alice := mirrorHarness(t)
	if err := insertSwarmGraph(ctx, h.testServer, "s1", []byte(mirrorGraph), "waiting"); err != nil {
		t.Fatal(err)
	}
	st := h.srv.store
	if _, err := st.AppendPipelineVisit(ctx, "s1", "s1", store.PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	requests, err := st.SwarmRequests(ctx, "s1")
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %+v %v", requests, err)
	}
	if _, err := st.AnswerSwarmRequest(ctx, store.SwarmAnswer{SwarmID: "s1", RequestID: requests[0].ID, UserID: alice, Outcome: "success"}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	telegramEventually(t, "three notifications reach the chat", func() bool { return len(h.api.sentTo(installBotToken, mirrorChat)) == 3 })
	texts := h.api.sentTo(installBotToken, mirrorChat)
	contains(t, "request", texts[0], `Swarm "release-train"`, "node ask asks the operator", "Ship build 42?", "- Ship", "https://brood.example/#/swarms/s1")
	contains(t, "answer", texts[1], "alice answered the request of node ask", "Answer:\nShip")
	contains(t, "pass end", texts[2], "succeeded")
	// Каждое уведомление записано отправленным с номером своего сообщения.
	rows := h.column(`SELECT group_concat(kind || ':' || state || ':' || telegram_message_id, ',') FROM (SELECT * FROM telegram_outbox ORDER BY id)`)
	if rows != "request_opened:sent:1,request_answered:sent:2,pass_finished:sent:3" {
		t.Fatalf("outbox after the sends: %s", rows)
	}
}

func TestSenderSendsOneAtATimeInQueueOrder(t *testing.T) {
	h, alice := mirrorHarness(t)
	var mu sync.Mutex
	active, most := 0, 0
	h.api.scriptSend(installBotToken, func(w http.ResponseWriter, r *http.Request, s fakeSent) bool {
		mu.Lock()
		active++
		most = max(most, active)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return false
	})
	var ids []int64
	for pass := 1; pass <= 5; pass++ {
		ids = append(ids, stopped(t, h, alice, pass))
	}
	h.settledAs(ids[4], "sent:1/1")
	texts := h.api.sentTo(installBotToken, mirrorChat)
	if len(texts) != 5 {
		t.Fatalf("%d messages, want 5", len(texts))
	}
	for i, text := range texts {
		contains(t, "message", text, "pass "+strconv.Itoa(i+1)+" was stopped")
	}
	mu.Lock()
	defer mu.Unlock()
	if most != 1 {
		t.Fatalf("%d sends to one user were in flight at once", most)
	}
}

func TestUncertainSendIsUnconfirmedAndNeverResent(t *testing.T) {
	h, alice := mirrorHarness(t)
	var mu sync.Mutex
	calls := 0
	h.api.scriptSend(installBotToken, func(w http.ResponseWriter, r *http.Request, s fakeSent) bool {
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(s.Text, "pass 1 ") {
			return false
		}
		calls++
		// Запрос дошёл, ответа нет: соединение рвётся до ответа.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return true
		}
		conn.Close()
		return true
	})
	first, second := stopped(t, h, alice, 1), stopped(t, h, alice, 2)
	h.settledAs(second, "sent:1/1")
	if got := h.outboxState(first); got != "unconfirmed:1/1" {
		t.Fatalf("uncertain send = %s", got)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 || h.outboxState(first) != "unconfirmed:1/1" {
		t.Fatalf("an unconfirmed notification was sent %d times: %s", calls, h.outboxState(first))
	}
}

func TestDefiniteRefusalFailsAndTheQueueGoesOn(t *testing.T) {
	h, alice := mirrorHarness(t)
	h.api.scriptSend(installBotToken, func(w http.ResponseWriter, r *http.Request, s fakeSent) bool {
		if strings.Contains(s.Text, "pass 1 ") {
			fakeRefuse(w, http.StatusForbidden, "Forbidden: bot was blocked by the user")
			return true
		}
		return false
	})
	first, second := stopped(t, h, alice, 1), stopped(t, h, alice, 2)
	h.settledAs(second, "sent:1/1")
	if got := h.outboxState(first); got != "failed:1/1" {
		t.Fatalf("refused send = %s", got)
	}
}

func TestRateLimitedSendWaitsAndIsSentOnce(t *testing.T) {
	h, alice := mirrorHarness(t)
	var mu sync.Mutex
	var calls []time.Time
	h.api.scriptSend(installBotToken, func(w http.ResponseWriter, r *http.Request, s fakeSent) bool {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, time.Now())
		if len(calls) == 1 {
			fakeRefuseRetry(w, http.StatusTooManyRequests, "Too Many Requests: retry after 1", 1)
			return true
		}
		return false
	})
	id := stopped(t, h, alice, 1)
	h.settledAs(id, "sent:2/2")
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[1].Sub(calls[0]) < 900*time.Millisecond {
		t.Fatalf("calls %v: want a second one after retry_after", calls)
	}
	if n := len(h.api.sentTo(installBotToken, mirrorChat)); n != 1 {
		t.Fatalf("%d messages in the chat, want 1", n)
	}
}

// Пока Telegram недостижим, уведомление, которое заведомо не ушло, держит
// голову очереди без предела попыток: предел попыток касается только сбоев
// чтения базы. Списывается failed только уведомление старше суток, и
// очередь идёт дальше.
func TestUnsentNotificationWaitsForTelegramUntilItIsStale(t *testing.T) {
	// Bot API, к которому нет соединения: каждая отправка заведомо не ушла.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	ln.Close()
	const limit = 3
	var alice string
	h := startTelegramHarness(t, newTestBotAPI(t), closed, installBotToken, func(ctx context.Context, st *store.Store) {
		alice = seedMirror(t, ctx, st)
	}, func(o *telegramOptions) { o.sendAttempts = limit })
	stale := outboxInsertCreated(t, h.dbPath, alice, "pass_finished", "swarm/s1/pass/1", `{"swarm":"s1","sequence":1,"execution":"stopped"}`,
		time.Now().Add(-telegramSendMaxAge-time.Hour))
	fresh := stopped(t, h, alice, 2)
	h.settledAs(stale, "failed:1/1")
	telegramEventually(t, "the fresh notification outlives the attempt limit in the queue", func() bool {
		if state := h.column(`SELECT state FROM telegram_outbox WHERE id = ?`, fresh); state != "pending" {
			t.Fatalf("fresh notification left the queue as %s while Telegram was unreachable", h.outboxState(fresh))
		}
		attempts, err := strconv.Atoi(h.column(`SELECT attempts FROM telegram_outbox WHERE id = ?`, fresh))
		if err != nil {
			t.Fatal(err)
		}
		return attempts > 3*limit
	})
}

func TestRecoveryAtStartupNeverResendsAnInterruptedAttempt(t *testing.T) {
	ctx := context.Background()
	// Прежний процесс начал попытку остановки прохода 1 и упал до исхода.
	dbPath := t.TempDir() + "/brood.db"
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	alice := seedMirror(t, ctx, st)
	interrupted := stoppedAt(t, dbPath, alice, 1)
	queued := stoppedAt(t, dbPath, alice, 2)
	if n, err := st.BeginTelegramAttempt(ctx, alice, true); err != nil || n.ID != interrupted {
		t.Fatalf("begin the interrupted attempt: %+v %v", n, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	api := newTestBotAPI(t)
	h := startTelegramHarnessAt(t, dbPath, api, "", installBotToken, nil)
	h.settledAs(queued, "sent:1/1")
	if got := h.outboxState(interrupted); got != "unconfirmed:1/1" {
		t.Fatalf("interrupted attempt after restart = %s", got)
	}
	for _, text := range api.sentTo(installBotToken, mirrorChat) {
		if strings.Contains(text, "pass 1 ") {
			t.Fatalf("the interrupted notification was sent again: %q", text)
		}
	}
}

// waitClosed ждёт, пока закроется ch.
func waitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting: %s", what)
	}
}

// closeServer закрывает сервер в своей горутине: Close ждёт отправителя, и
// тест тем временем отпускает отправку. Закрыт канал — Close вернулся.
func (h *telegramHarness) closeServer() <-chan struct{} {
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		h.srv.Close()
	}()
	return closed
}

// holdSend держит отправки бота установки, пока тест не закроет release или
// клиент не оборвёт запрос; held закрывается при первой отправке.
func (h *telegramHarness) holdSend(release <-chan struct{}) (held <-chan struct{}) {
	started := make(chan struct{})
	var once sync.Once
	h.api.scriptSend(installBotToken, func(w http.ResponseWriter, r *http.Request, s fakeSent) bool {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return false
		case <-r.Context().Done():
			return true
		}
	})
	return started
}

// sentRows — исходы уведомлений по порядку id как
// «state:attempts/settled_attempts:telegram_message_id».
func (h *telegramHarness) sentRows() string {
	h.t.Helper()
	return h.column(`SELECT group_concat(state || ':' || attempts || '/' || settled_attempts || ':' || coalesce(telegram_message_id, ''), ',') FROM (SELECT * FROM telegram_outbox ORDER BY id)`)
}

func TestSendInFlightAtShutdownIsSettledSentWithItsMessage(t *testing.T) {
	h, alice := mirrorHarness(t)
	telegramEventually(t, "the installation bot polls", func() bool { return h.poller("") != nil })
	poller := h.poller("")
	release := make(chan struct{})
	held := h.holdSend(release)
	stopped(t, h, alice, 1)
	waitClosed(t, "the send is in flight", held)
	closed := h.closeServer()
	// Поллер останавливает отмена контекста сервера: остановка началась.
	telegramEventually(t, "the server cancels its work", func() bool { return !poller.alive() })
	select {
	case <-closed:
		t.Fatal("Close returned while a send was in flight")
	default:
	}
	close(release)
	waitClosed(t, "Close returns", closed)
	if got := h.sentRows(); got != "sent:1/1:1" {
		t.Fatalf("send that ended during shutdown = %s, want sent with its message", got)
	}
}

func TestSendHeldPastTheShutdownGraceIsCutAndUnconfirmed(t *testing.T) {
	h, alice := mirrorHarness(t, func(o *telegramOptions) { o.grace = 100 * time.Millisecond })
	never := make(chan struct{})
	held := h.holdSend(never)
	stopped(t, h, alice, 1)
	waitClosed(t, "the send is in flight", held)
	waitClosed(t, "Close returns after the grace", h.closeServer())
	if got := h.sentRows(); got != "unconfirmed:1/1:" {
		t.Fatalf("send cut at shutdown = %s, want unconfirmed", got)
	}
}

func TestFailedSettleIsWrittenAgainAndTheQueueGoesOn(t *testing.T) {
	failed, dropped := make(chan struct{}), make(chan struct{})
	drop := sync.OnceFunc(func() { close(dropped) })
	var once sync.Once
	h, alice := mirrorHarness(t, func(o *telegramOptions) {
		o.backoff = func(int) time.Duration {
			// Отправки идут, и первая пауза отправителя — за отказом записи
			// исхода: работник ждёт, пока тест снимает сбой.
			once.Do(func() {
				close(failed)
				<-dropped
			})
			return 5 * time.Millisecond
		}
	})
	t.Cleanup(drop)
	h.sql(settleFails)
	first, second := stopped(t, h, alice, 1), stopped(t, h, alice, 2)
	waitClosed(t, "the settle of a sent notification fails", failed)
	if got := h.outboxState(first); got != "pending:1/0" {
		t.Fatalf("attempt whose settle failed = %s, want in flight", got)
	}
	h.sql(`DROP TRIGGER telegram_settle_fails`)
	drop()
	h.settledAs(second, "sent:1/1")
	if got := h.sentRows(); got != "sent:1/1:1,sent:1/1:2" {
		t.Fatalf("outbox after the failed settle = %s, want both sent with their messages", got)
	}
	if n := len(h.api.sentTo(installBotToken, mirrorChat)); n != 2 {
		t.Fatalf("%d messages in the chat, want 2: a failed settle never resends", n)
	}
}

func TestSenderRetiresTheQueueOfAnUnlinkedUser(t *testing.T) {
	ctx := context.Background()
	h, alice := mirrorHarness(t)
	if _, err := h.srv.store.UnlinkTelegram(ctx, alice); err != nil {
		t.Fatal(err)
	}
	id := stopped(t, h, alice, 1)
	h.settledAs(id, "failed:1/1")
	if n := len(h.api.sent(installBotToken)); n != 0 {
		t.Fatalf("%d messages sent for an unlinked user", n)
	}
}

func TestLongMaterialReachesTheChatAsADocument(t *testing.T) {
	ctx := context.Background()
	h, alice := mirrorHarness(t)
	if err := insertSwarmGraph(ctx, h.testServer, "s1", []byte(mirrorGraph), "waiting"); err != nil {
		t.Fatal(err)
	}
	if err := insertSwarmStep(ctx, h.testServer, "s1", store.PipelineVisit{Sequence: 1, NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	material := strings.Repeat("A line of the step report.\n", 400)
	h.sql(`INSERT INTO swarm_requests (id, swarm_id, sequence, origin, kind, question, context, options, state, created_at)
		VALUES ('r-long', 's1', 1, 'node', 'choice', 'Ship build 42?', ?, '[{"label":"Ship","outcome":"success"}]', 'open', 'now')`, material)
	id := outboxInsert(t, h.dbPath, alice, "request_opened", "swarm/s1/request/r-long", `{"swarm":"s1","request":"r-long","sequence":1,"node":"ask"}`)
	h.settledAs(id, "sent:1/1")
	sent := h.api.sent(installBotToken)
	if len(sent) != 1 || sent[0].ChatID != mirrorChat || sent[0].Name == "" || !strings.Contains(sent[0].Document, material) {
		t.Fatalf("document: %+v", sent)
	}
	contains(t, "caption", sent[0].Text, `Swarm "release-train"`, "attached")
}

// Задачу прохода и причину отказа шага строка очереди не несёт: отправитель
// читает их из шага, который называет событие; шага нет — сообщать не о чем,
// failed без отправки.
func TestSenderReadsTheGoalAndTheReasonFromTheStep(t *testing.T) {
	h, alice := mirrorHarness(t)
	h.sql(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,goal,source_commit) VALUES ('s1',1,'ask','answered','Ship issue #7: the parser drops comments.','0123456789abcdef0123456789abcdef01234567')`)
	h.sql(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state,finished_at,detail) VALUES ('s1',2,'build','refused','now','role build has no live bound actor')`)
	started := outboxInsert(t, h.dbPath, alice, "pass_started", "swarm/s1/pass/1", `{"swarm":"s1","sequence":1,"node":"ask","execution":"waiting"}`)
	refused := outboxInsert(t, h.dbPath, alice, "step_failed", "swarm/s1/step/2", `{"swarm":"s1","sequence":2,"node":"build","state":"refused"}`)
	vanished := outboxInsert(t, h.dbPath, alice, "step_failed", "swarm/s1/step/9", `{"swarm":"s1","sequence":9,"node":"build","state":"refused"}`)
	h.settledAs(vanished, "failed:1/1")
	for _, id := range []int64{started, refused} {
		if got := h.outboxState(id); got != "sent:1/1" {
			t.Fatalf("notification %d = %s, want sent", id, got)
		}
	}
	texts := h.api.sentTo(installBotToken, mirrorChat)
	if len(texts) != 2 {
		t.Fatalf("%d messages, want the pass start and the refused step: %q", len(texts), texts)
	}
	contains(t, "pass start", texts[0], "pass 1 started", "Goal:\nShip issue #7: the parser drops comments.")
	contains(t, "refused step", texts[1], "step 2 (node build) was refused", "Reason:\nrole build has no live bound actor")
}

func TestRetentionDeletesSettledNotifications(t *testing.T) {
	h, alice := mirrorHarness(t, func(o *telegramOptions) {
		o.retention = time.Millisecond
		o.prune = 20 * time.Millisecond
	})
	id := stopped(t, h, alice, 1)
	telegramEventually(t, "the sent notification is deleted", func() bool {
		return h.column(`SELECT count(*) FROM telegram_outbox WHERE id = ?`, id) == "0"
	})
	if n := len(h.api.sentTo(installBotToken, mirrorChat)); n != 1 {
		t.Fatalf("%d messages, want the notification sent before retention", n)
	}
}
