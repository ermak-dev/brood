package server

// FEATURE: Swarm and actor commands from a linked Telegram chat (#284, epic
// #278).
// SPEC: a linked private chat drives brood as the user of its link, with
// that user's rights: /swarms, /swarm, /requests, /use, /actors, /start,
// /stop, /actor and /help. Each command goes through the same server methods
// as the panel and bro: the app rights, the swarm store methods with the
// telegram session checked in their write transaction, the command catalog.
// A swarm the user does not read is not found, as in the API. /use keeps the
// default swarm of the chat in the link row, across restarts; /requests,
// /swarm and /actors use it when they name no swarm. /start, /stop and
// /actor run only after a button press: a confirmation has a short id in
// its callback data and a TTL; a press after the TTL, a cancel, a foreign or
// unknown id and a press on a chat that lost its link run nothing, and a
// repeated callback, the same press again or a second press, runs the action
// at most once. A press accepted within the TTL starts its action no later
// than a short grace after the TTL; an action the Bot API calls before it
// delayed longer runs nothing. A press from a group, from a bot or from
// another sender than the person of the private chat is ignored. A command the user has no right for is refused before any
// confirmation with the role it needs; a right lost before the press is
// refused by the write itself. A confirmation acts only under the link
// session it was issued under: a link that ended or was made again before
// the press runs nothing and gets no reply, and /start, /stop and /use check
// that session in their write transaction. A pressed action runs within the
// command timeout. A swarm the user does not read is not found by its id
// either. An unknown command gets a short refusal. A refusal
// carries the public reason of the underlying refusal (#276), never the text
// of an internal error. Replies stay within the Bot API message limit: long
// lists are paged.
// IMPLEMENTATION: telegramCommands (telegram_commands.go), confirmations
// (telegram_confirm.go), the poller (telegram_bots.go), SetTelegramSwarm.
// STATUS: DONE

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

// telegramTestChat — личный чат привязанного пользователя у бота token.
type telegramTestChat struct {
	t     *testing.T
	api   *fakeBotAPI
	token string
	id    int64
}

// deliverUpdate ставит обновление в очередь бота и ждёт, пока поллер его
// обработает: ответы, которые поллер шлёт при обработке, к этому моменту
// записаны.
func deliverUpdate(t *testing.T, api *fakeBotAPI, token string, update map[string]any) {
	t.Helper()
	id := api.push(token, update)
	telegramEventually(t, "the poller confirms the update", func() bool { return api.confirmed(token, id) })
}

// messages — сообщения бота в этот чат.
func (c telegramTestChat) messages() []fakeSent {
	var out []fakeSent
	for _, s := range c.api.sent(c.token) {
		if s.ChatID == c.id {
			out = append(out, s)
		}
	}
	return out
}

// send шлёт текст и возвращает сообщения, которые бот ответил на него.
func (c telegramTestChat) send(text string) []fakeSent {
	c.t.Helper()
	before := len(c.messages())
	deliverUpdate(c.t, c.api, c.token, privateMessage(c.id, "person", text))
	return c.messages()[before:]
}

// reply — единственный ответ бота на текст.
func (c telegramTestChat) reply(text string) fakeSent {
	c.t.Helper()
	got := c.send(text)
	if len(got) != 1 {
		c.t.Fatalf("%q got %d replies, want one: %+v", text, len(got), got)
	}
	return got[0]
}

// text — текст единственного ответа без клавиатуры.
func (c telegramTestChat) text(command string) string {
	c.t.Helper()
	got := c.reply(command)
	if got.Markup != nil {
		c.t.Fatalf("%q replied with buttons: %+v", command, got)
	}
	return got.Text
}

// confirmation — ответ с кнопками подтверждения: первая подтверждает,
// вторая отменяет.
func (c telegramTestChat) confirmation(command string) fakeSent {
	c.t.Helper()
	got := c.reply(command)
	if got.Markup == nil || len(got.Markup.InlineKeyboard) != 1 || len(got.Markup.InlineKeyboard[0]) != 2 {
		c.t.Fatalf("%q did not ask for confirmation: %+v", command, got)
	}
	return got
}

// pressed — что бот сделал на нажатие: уведомление нажатия, правки
// клавиатуры и новые сообщения в чат.
type pressed struct {
	answer string
	edits  []fakeEdit
	sent   []fakeSent
}

// press нажимает кнопку button сообщения message; id — id нажатия.
func (c telegramTestChat) press(id string, message fakeSent, button int) pressed {
	c.t.Helper()
	return c.pressData(id, message.ID, message.Markup.InlineKeyboard[0][button].CallbackData)
}

// pressData нажимает кнопку с данными data под сообщением message.
func (c telegramTestChat) pressData(id string, message int64, data string) pressed {
	c.t.Helper()
	answers, edits, sent := len(c.api.answers(c.token)), len(c.api.edits(c.token)), len(c.messages())
	deliverUpdate(c.t, c.api, c.token, callbackPress(id, c.id, message, data))
	var got pressed
	for _, a := range c.api.answers(c.token)[answers:] {
		if a.ID == id {
			got.answer = a.Text
		}
	}
	got.edits = c.api.edits(c.token)[edits:]
	got.sent = c.messages()[sent:]
	return got
}

// only — единственное новое сообщение нажатия.
func (p pressed) only(t *testing.T) string {
	t.Helper()
	if len(p.sent) != 1 {
		t.Fatalf("the press sent %d messages, want one: %+v", len(p.sent), p.sent)
	}
	return p.sent[0].Text
}

// keyboardRemoved: нажатие сняло клавиатуру сообщения message.
func (p pressed) keyboardRemoved(message fakeSent) bool {
	for _, e := range p.edits {
		if e.MessageID == message.ID && e.ChatID == message.ChatID && e.Markup == nil {
			return true
		}
	}
	return false
}

func expectText(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s:\n got %q\nwant %q", what, got, want)
	}
}

// Чаты команд: все привязаны через бот установки.
const (
	aliceChat = 501
	bobChat   = 502
	carolChat = 503
	daveChat  = 504
)

// commandsHarness — сервер с ботом установки: проект proj, рои s1
// «release-train» (alice owner, bob viewer, dave editor) и s2 «docs»
// (alice owner); carol — участник проекта без роли. Уведомления выключены:
// в чат приходят только ответы на команды.
type commandsHarness struct {
	*telegramHarness
	alice, bob, carol, dave string
}

func newCommandsHarness(t *testing.T, seed ...func(ctx context.Context, st *store.Store, h *commandsHarness)) *commandsHarness {
	t.Helper()
	c := &commandsHarness{}
	c.telegramHarness = startTelegramHarness(t, newTestBotAPI(t), "", installBotToken, func(ctx context.Context, st *store.Store) {
		c.alice = seedMirror(t, ctx, st)
		if err := st.CreateSwarm(ctx, store.Swarm{ID: "s2", ProjectID: "p", OwnerID: c.alice, Name: "docs", AtespaceName: "s2", State: "active"}); err != nil {
			t.Fatal(err)
		}
		for name, role := range map[string]string{"bob": "viewer", "carol": "", "dave": "editor"} {
			id := createTelegramUser(t, ctx, st, name)
			if err := st.AddMember(ctx, "p", id); err != nil {
				t.Fatal(err)
			}
			if role != "" {
				if err := st.SetSwarmMember(ctx, "s1", id, role); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "bob":
				c.bob = id
			case "carol":
				c.carol = id
			case "dave":
				c.dave = id
			}
		}
		for user, chat := range map[string]int64{c.bob: bobChat, c.carol: carolChat, c.dave: daveChat} {
			linkInStore(t, ctx, st, user, chat)
		}
		for _, user := range []string{c.alice, c.bob, c.carol, c.dave} {
			if _, err := st.SetTelegramEvents(ctx, user, []string{}); err != nil {
				t.Fatal(err)
			}
		}
		for _, f := range seed {
			f(ctx, st, c)
		}
	})
	return c
}

func (c *commandsHarness) chat(id int64) telegramTestChat {
	return telegramTestChat{t: c.t, api: c.api, token: installBotToken, id: id}
}

// waiting ставит s1 ждать оператора на узле ask: шаг 1 и открытый запрос.
func (c *commandsHarness) waiting() {
	c.t.Helper()
	ctx := context.Background()
	if err := insertSwarmGraph(ctx, c.testServer, "s1", []byte(mirrorGraph), "waiting"); err != nil {
		c.t.Fatal(err)
	}
	// insertSwarmGraph пишет updated_at словом, а список роёв app читает
	// момент.
	c.sql(`UPDATE swarms SET updated_at = ? WHERE id = 's1'`, time.Now().UTC().Format(time.RFC3339Nano))
	if _, err := c.srv.store.AppendPipelineVisit(ctx, "s1", "s1", store.PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		c.t.Fatal(err)
	}
}

func (c *commandsHarness) execution(swarm string) string {
	c.t.Helper()
	sw, err := c.srv.store.GetSwarm(context.Background(), swarm)
	if err != nil {
		c.t.Fatal(err)
	}
	return sw.Execution
}

func TestTelegramHelpListsTheCommands(t *testing.T) {
	h := newCommandsHarness(t)
	help := h.chat(aliceChat).text("/help")
	for _, command := range []string{"/swarms", "/swarm", "/use", "/requests", "/actors", "/start <swarm> <goal>", "/stop <swarm>", "/actor <swarm> <name> stop|start|update|reset", "/help", "expires after 2 minutes"} {
		if !strings.Contains(help, command) {
			t.Fatalf("help does not name %q:\n%s", command, help)
		}
	}
	if got := h.chat(aliceChat).text("/help@brood_bot"); got != help {
		t.Fatalf("help addressed to the bot = %q", got)
	}
}

func TestTelegramUnknownCommandIsRefusedShortly(t *testing.T) {
	h := newCommandsHarness(t)
	chat := h.chat(aliceChat)
	expectText(t, "unknown command", chat.text("/status"), "Unknown command /status. Send /help for the commands.")
	long := "/" + strings.Repeat("x", 300)
	if got := chat.text(long); len(got) > 120 || !strings.HasSuffix(got, "Send /help for the commands.") {
		t.Fatalf("refusal of a long unknown command = %q", got)
	}
	// Текст без команды — не команда: им отвечают на запрос (#283), и без
	// reply на сообщение запроса чат получает подсказку, как ответить.
	expectText(t, "plain text", chat.text("hello"), telegramReplyTextUsage)
}

func TestTelegramSwarmsListsTheSwarmsTheUserReads(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	expectText(t, "alice /swarms", h.chat(aliceChat).text("/swarms"),
		"Your swarms, page 1 of 1:\n- docs (proj): idle\n- release-train (proj): waiting, 1 open request")
	expectText(t, "bob /swarms", h.chat(bobChat).text("/swarms"),
		"Your swarms, page 1 of 1:\n- release-train (proj): waiting, 1 open request")
	expectText(t, "carol /swarms", h.chat(carolChat).text("/swarms"), "You have no swarms in brood.")
}

func TestTelegramSwarmShowsExecutionStepAndOpenRequests(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	expectText(t, "/swarm release-train", h.chat(bobChat).text("/swarm release-train"),
		"Swarm \"release-train\" (project proj), your role: viewer\nExecution: waiting\nWorkflow: test v1\nCurrent step: 1, node ask, waiting\nOpen requests: 1\nManaged actors: 0")
	expectText(t, "/swarm docs", h.chat(aliceChat).text("/swarm docs"),
		"Swarm \"docs\" (project proj), your role: owner\nExecution: idle\nNo pass has run yet.\nOpen requests: 0\nManaged actors: 0")
	// Рой без роли скрыт так же, как отсутствующий.
	expectText(t, "carol /swarm", h.chat(carolChat).text("/swarm release-train"),
		`No swarm "release-train" among your swarms. Send /swarms for the list.`)
	expectText(t, "unknown swarm", h.chat(aliceChat).text("/swarm nope"),
		`No swarm "nope" among your swarms. Send /swarms for the list.`)
	expectText(t, "/swarm without a default", h.chat(aliceChat).text("/swarm"),
		"No default swarm: choose one with /use <swarm>.")
	// По id и по <проект>/<имя>.
	if got := h.chat(aliceChat).text("/swarm s1"); !strings.HasPrefix(got, `Swarm "release-train"`) {
		t.Fatalf("/swarm by id = %q", got)
	}
	if got := h.chat(aliceChat).text("/swarm proj/docs"); !strings.HasPrefix(got, `Swarm "docs"`) {
		t.Fatalf("/swarm by project and name = %q", got)
	}
}

// Рой, который пользователь не читает, не найден и по id: ни чтение, ни
// изменение не называют его задачу, шаг и запросы и не просят подтверждения.
func TestTelegramSwarmTheUserDoesNotReadIsNotFoundByItsID(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(carolChat)
	notFound := `No swarm "s1" among your swarms. Send /swarms for the list.`
	for _, command := range []string{"/swarm s1", "/actors s1", "/use s1", "/stop s1", "/start s1 Ship it", "/actor s1 coding stop"} {
		expectText(t, "carol "+command, chat.text(command), notFound)
	}
	if got := h.link(h.carol).DefaultSwarmID; got != "" {
		t.Fatalf("/use of a hidden swarm wrote %q", got)
	}
	if h.execution("s1") != "waiting" {
		t.Fatalf("a command on a hidden swarm changed it: %s", h.execution("s1"))
	}
}

func TestTelegramSwarmNamesRepeatedAcrossProjectsAreAskedToBeQualified(t *testing.T) {
	h := newCommandsHarness(t, func(ctx context.Context, st *store.Store, c *commandsHarness) {
		if err := st.CreateProject(ctx, store.Project{ID: "q", Name: "other"}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddMember(ctx, "q", c.alice); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSwarm(ctx, store.Swarm{ID: "s3", ProjectID: "q", OwnerID: c.alice, Name: "docs", AtespaceName: "s3", State: "active"}); err != nil {
			t.Fatal(err)
		}
	})
	chat := h.chat(aliceChat)
	expectText(t, "ambiguous name", chat.text("/swarm docs"),
		`Several of your swarms are named "docs": name one as other/docs or proj/docs.`)
	if got := chat.text("/swarm other/docs"); !strings.HasPrefix(got, `Swarm "docs" (project other)`) {
		t.Fatalf("/swarm other/docs = %q", got)
	}
}

func TestTelegramUseKeepsTheDefaultSwarmForRequests(t *testing.T) {
	dbPath := t.TempDir() + "/brood.db"
	api := newTestBotAPI(t)
	var alice string
	first := startTelegramHarnessAt(t, dbPath, api, "", installBotToken, func(ctx context.Context, st *store.Store) {
		alice = seedMirror(t, ctx, st)
		if _, err := st.SetTelegramEvents(ctx, alice, []string{}); err != nil {
			t.Fatal(err)
		}
	})
	h := &commandsHarness{telegramHarness: first, alice: alice}
	h.waiting()
	chat := h.chat(aliceChat)
	expectText(t, "/requests without a default", chat.text("/requests"), "No default swarm: choose one with /use <swarm>.")
	expectText(t, "/use without a default", chat.text("/use"), "No default swarm: choose one with /use <swarm>.")
	expectText(t, "/use release-train", chat.text("/use release-train"),
		`Swarm "release-train" (project proj) is the default swarm of this chat now.`)
	if got := h.link(h.alice).DefaultSwarmID; got != "s1" {
		t.Fatalf("default swarm in the link row = %q", got)
	}
	want := "Open requests of swarm \"release-train\", page 1 of 1:\n- node ask, step 1 (choice): Ship build 42?\nAnswer them in brood: https://brood.example/#/swarms/s1"
	expectText(t, "/requests", chat.text("/requests"), want)
	expectText(t, "/use", chat.text("/use"), `The default swarm of this chat is "release-train" (project proj).`)
	// Рой по умолчанию переживает перезапуск сервера: он в строке привязки.
	first.http.Close()
	if err := first.srv.Close(); err != nil {
		t.Fatal(err)
	}
	startTelegramHarnessAt(t, dbPath, api, "", installBotToken, nil)
	restarted := telegramTestChat{t: t, api: api, token: installBotToken, id: aliceChat}
	expectText(t, "/requests after a restart", restarted.text("/requests"), want)
	if got := restarted.text("/swarm"); !strings.HasPrefix(got, `Swarm "release-train"`) {
		t.Fatalf("/swarm with the default after a restart = %q", got)
	}
}

func TestTelegramUseRefusesASwarmTheUserDoesNotRead(t *testing.T) {
	h := newCommandsHarness(t)
	expectText(t, "carol /use", h.chat(carolChat).text("/use release-train"),
		`No swarm "release-train" among your swarms. Send /swarms for the list.`)
	if got := h.link(h.carol).DefaultSwarmID; got != "" {
		t.Fatalf("a refused /use wrote %q", got)
	}
	// Viewer читает рой и выбирает его.
	expectText(t, "bob /use", h.chat(bobChat).text("/use release-train"),
		`Swarm "release-train" (project proj) is the default swarm of this chat now.`)
	// Право читать потеряно: рой по умолчанию больше не годится, а не
	// показывается.
	if err := h.srv.store.RemoveSwarmMember(context.Background(), "s1", h.bob); err != nil {
		t.Fatal(err)
	}
	expectText(t, "/requests after the role was removed", h.chat(bobChat).text("/requests"),
		"Your default swarm is no longer available to you: choose another with /use <swarm>.")
}

func TestTelegramRequestsListsOnlyOpenRequests(t *testing.T) {
	h := newCommandsHarness(t)
	chat := h.chat(aliceChat)
	chat.text("/use docs")
	expectText(t, "no open requests", chat.text("/requests"), `Swarm "docs" has no open requests.`)
	h.waiting()
	chat.text("/use release-train")
	requests, err := h.srv.store.SwarmRequests(context.Background(), "s1")
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %+v %v", requests, err)
	}
	if _, err := h.srv.store.AnswerSwarmRequest(context.Background(), store.SwarmAnswer{SwarmID: "s1", RequestID: requests[0].ID, UserID: h.alice, Outcome: "success"}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	expectText(t, "answered request", chat.text("/requests"), `Swarm "release-train" has no open requests.`)
}

func TestTelegramListsArePagedWithinTheMessageLimit(t *testing.T) {
	long := strings.Repeat("n", 900)
	h := newCommandsHarness(t, func(ctx context.Context, st *store.Store, c *commandsHarness) {
		for i := range 43 {
			name := fmt.Sprintf("swarm-%02d", i)
			if i < 3 {
				name = fmt.Sprintf("%s-%s", name, long)
			}
			id := fmt.Sprintf("bulk-%02d", i)
			if err := st.CreateSwarm(ctx, store.Swarm{ID: id, ProjectID: "p", OwnerID: c.alice, Name: name, AtespaceName: id, State: "active"}); err != nil {
				t.Fatal(err)
			}
		}
	})
	chat := h.chat(aliceChat)
	first := chat.text("/swarms")
	if !strings.HasPrefix(first, "Your swarms, page 1 of 3:\n") || !strings.HasSuffix(first, "\nNext page: /swarms 2") {
		t.Fatalf("first page:\n%s", first)
	}
	if lines := strings.Count(first, "\n- "); lines != telegramPageSize {
		t.Fatalf("first page has %d swarms, want %d", lines, telegramPageSize)
	}
	last := chat.text("/swarms 3")
	if !strings.HasPrefix(last, "Your swarms, page 3 of 3:\n") || strings.Contains(last, "Next page") {
		t.Fatalf("last page:\n%s", last)
	}
	// 45 роёв: 20, 20 и 5.
	if lines := strings.Count(last, "\n- "); lines != 5 {
		t.Fatalf("last page has %d swarms, want 5", lines)
	}
	for _, page := range []string{first, chat.text("/swarms 2"), last} {
		if telegram.TextLength(page) > telegram.MessageLimit {
			t.Fatalf("a page exceeds the message limit: %d", telegram.TextLength(page))
		}
		for _, line := range strings.Split(page, "\n") {
			if telegram.TextLength(line) > telegramLineLimit {
				t.Fatalf("a list line exceeds its limit: %d", telegram.TextLength(line))
			}
		}
	}
	expectText(t, "page past the end", chat.text("/swarms 4"), "There is no page 4: the list has 3 pages.")
	expectText(t, "not a page", chat.text("/swarms two"), "The page must be a number from 1: /swarms [page].")
}

func TestTelegramStopRunsOnlyAfterConfirmation(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(aliceChat)
	ask := chat.confirmation("/stop release-train")
	expectText(t, "confirmation", ask.Text,
		"Stop the running pass of swarm \"release-train\" (project proj)? Its open request is cancelled; actors, workspaces and role bindings stay.\n\nConfirm within 2 minutes.")
	if ask.Markup.InlineKeyboard[0][0].Text != "Stop pass" || ask.Markup.InlineKeyboard[0][1].Text != "Cancel" {
		t.Fatalf("buttons: %+v", ask.Markup)
	}
	for _, button := range ask.Markup.InlineKeyboard[0] {
		if len(button.CallbackData) > 64 {
			t.Fatalf("callback data of %d bytes", len(button.CallbackData))
		}
	}
	if h.execution("s1") != "waiting" {
		t.Fatal("the swarm stopped before the confirmation")
	}
	got := chat.press("q1", ask, 0)
	expectText(t, "press answer", got.answer, "Running.")
	if !got.keyboardRemoved(ask) {
		t.Fatalf("the confirmation kept its buttons: %+v", got.edits)
	}
	expectText(t, "result", got.only(t), `Swarm "release-train" stopped: execution stopped, last step 1 at node ask (stopped).`)
	if h.execution("s1") != "stopped" {
		t.Fatalf("execution after the confirmation = %s", h.execution("s1"))
	}
	// Повтор того же нажатия (Telegram прислал его снова) и второе нажатие
	// действие не повторяют.
	again := chat.press("q1", ask, 0)
	expectText(t, "repeated callback", again.answer, "This action already ran; it is not repeated.")
	second := chat.press("q2", ask, 0)
	expectText(t, "second press", second.answer, "This action already ran; it is not repeated.")
	cancel := chat.press("q3", ask, 1)
	expectText(t, "cancel after the run", cancel.answer, "This action already ran; it is not repeated.")
	if len(again.sent)+len(second.sent)+len(cancel.sent) != 0 {
		t.Fatalf("repeats sent messages: %+v %+v %+v", again.sent, second.sent, cancel.sent)
	}
}

func TestTelegramConfirmationExpiresIntoNothing(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(aliceChat)
	ask := chat.confirmation("/stop release-train")
	h.clock.Advance(telegramConfirmTTL + time.Second)
	got := chat.press("q1", ask, 0)
	expectText(t, "expired press", got.answer, "This confirmation expired; nothing was done.")
	if !got.keyboardRemoved(ask) || len(got.sent) != 0 {
		t.Fatalf("expired press: %+v", got)
	}
	if h.execution("s1") != "waiting" {
		t.Fatalf("an expired confirmation ran: %s", h.execution("s1"))
	}
	again := chat.press("q2", ask, 0)
	expectText(t, "press after expiry", again.answer, "This confirmation expired; nothing was done.")
	if h.execution("s1") != "waiting" {
		t.Fatal("a second press after expiry ran the action")
	}
	// Пока срок не вышел, подтверждение действует.
	fresh := chat.confirmation("/stop release-train")
	h.clock.Advance(telegramConfirmTTL - time.Second)
	if got := chat.press("q3", fresh, 0); got.answer != "Running." || h.execution("s1") != "stopped" {
		t.Fatalf("a confirmation within its TTL: %+v, execution %s", got, h.execution("s1"))
	}
}

// Нажатие, принятое в срок, начинает действие не позже отсрочки после срока:
// ответ нажатию и снятие клавиатуры идут до действия, и действие, которое
// они задержали дольше отсрочки, не исполняется.
func TestTelegramPressInTimeDoesNotStartPastTheGrace(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(aliceChat)
	ask := chat.confirmation("/stop release-train")
	h.clock.Advance(telegramConfirmTTL - time.Second)
	h.api.scriptAnswer(installBotToken, func() { h.clock.Advance(telegramConfirmGrace + time.Second) })
	got := chat.press("q1", ask, 0)
	expectText(t, "late press answer", got.answer, "Running.")
	expectText(t, "late start", got.only(t), "This confirmation expired before its action could start; nothing was done. Send the command again.")
	if h.execution("s1") != "waiting" {
		t.Fatalf("an action started past the grace: %s", h.execution("s1"))
	}
	h.api.scriptAnswer(installBotToken, nil)
	again := chat.press("q2", ask, 0)
	expectText(t, "press after a late start", again.answer, "This confirmation expired; nothing was done.")
	// В пределах отсрочки действие начинается.
	fresh := chat.confirmation("/stop release-train")
	h.clock.Advance(telegramConfirmTTL - time.Second)
	h.api.scriptAnswer(installBotToken, func() { h.clock.Advance(telegramConfirmGrace - time.Second) })
	if got := chat.press("q3", fresh, 0); got.answer != "Running." || h.execution("s1") != "stopped" {
		t.Fatalf("a press whose answer stayed within the grace: %+v, execution %s", got, h.execution("s1"))
	}
}

func TestTelegramCancelRunsNothing(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(aliceChat)
	ask := chat.confirmation("/stop release-train")
	got := chat.press("q1", ask, 1)
	expectText(t, "cancel", got.answer, "Cancelled; nothing was done.")
	if !got.keyboardRemoved(ask) || len(got.sent) != 0 {
		t.Fatalf("cancel: %+v", got)
	}
	confirm := chat.press("q2", ask, 0)
	expectText(t, "confirm after cancel", confirm.answer, "This confirmation was cancelled; nothing was done.")
	if h.execution("s1") != "waiting" {
		t.Fatal("a cancelled confirmation ran")
	}
}

func TestTelegramConfirmationBelongsToItsChat(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	ask := h.chat(aliceChat).confirmation("/stop release-train")
	data := ask.Markup.InlineKeyboard[0][0].CallbackData
	// dave вправе остановить рой, но нажатие подтверждения alice из своего
	// чата не исполняет её команду.
	got := h.chat(daveChat).pressData("q1", ask.ID, data)
	expectText(t, "foreign press", got.answer, "This button is no longer valid; nothing was done.")
	for _, bad := range []string{"c:unknown:y", "c:", strings.Replace(data, ":y", ":z", 1)} {
		got := h.chat(aliceChat).pressData("q-"+bad, ask.ID, bad)
		expectText(t, "press of "+bad, got.answer, "This button is no longer valid; nothing was done.")
	}
	// Кнопка без префикса подтверждения — не подтверждение: её разбирают
	// ответы на запросы (#283), и чужие данные для них недействительны.
	other := h.chat(aliceChat).pressData("q-x", ask.ID, "x:"+data)
	expectText(t, "press of x:"+data, other.answer, telegramReplyButtonStale)
	if h.execution("s1") != "waiting" {
		t.Fatal("a foreign or malformed press ran the action")
	}
	// Отвязанный чат нажатием ничего не исполняет и ответа не получает.
	if _, err := h.srv.store.UnlinkTelegram(context.Background(), h.alice); err != nil {
		t.Fatal(err)
	}
	unlinked := h.chat(aliceChat).press("q2", ask, 0)
	if unlinked.answer != "" || len(unlinked.sent) != 0 || h.execution("s1") != "waiting" {
		t.Fatalf("an unlinked chat ran the confirmation: %+v, %s", unlinked, h.execution("s1"))
	}
}

// Нажатие действует только из личного чата человека: нажатие из группы, от
// бота и от собеседника не этого чата остаётся без ответа, клавиатуру не
// трогает и подтверждение не забирает.
func TestTelegramPressesFromGroupsBotsAndForeignSendersAreIgnored(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(aliceChat)
	ask := chat.confirmation("/stop release-train")
	data := ask.Markup.InlineKeyboard[0][0].CallbackData
	answers, edits, sent := len(h.api.answers(installBotToken)), len(h.api.edits(installBotToken)), len(h.api.sent(installBotToken))
	for _, press := range []map[string]any{
		// Группа, в которой нажимает alice.
		callbackPressIn("q-group", -100777, "group", aliceChat, false, ask.ID, data),
		// Чат не личный, хотя его id — id привязанного чата: нажатие отсекает
		// вид чата, а не поиск привязки.
		callbackPressIn("q-supergroup", aliceChat, "supergroup", aliceChat, false, ask.ID, data),
		// Бот с id привязанного собеседника.
		callbackPressIn("q-bot", aliceChat, "private", aliceChat, true, ask.ID, data),
		// Собеседник не этого чата.
		callbackPressIn("q-foreign", aliceChat, "private", daveChat, false, ask.ID, data),
	} {
		deliverUpdate(t, h.api, installBotToken, press)
	}
	if got := h.api.answers(installBotToken)[answers:]; len(got) != 0 {
		t.Fatalf("ignored presses were answered: %+v", got)
	}
	if got := h.api.edits(installBotToken)[edits:]; len(got) != 0 {
		t.Fatalf("ignored presses edited a keyboard: %+v", got)
	}
	if got := h.api.sent(installBotToken)[sent:]; len(got) != 0 {
		t.Fatalf("ignored presses sent messages: %+v", got)
	}
	if h.execution("s1") != "waiting" {
		t.Fatalf("an ignored press ran the action: %s", h.execution("s1"))
	}
	// Подтверждение осталось открытым: нажатие из самого чата его исполняет.
	if got := chat.press("q1", ask, 0); got.answer != "Running." || h.execution("s1") != "stopped" {
		t.Fatalf("the press of the chat after ignored presses: %+v, execution %s", got, h.execution("s1"))
	}
}

func TestTelegramStopNeedsTheManageRight(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	expectText(t, "viewer /stop", h.chat(bobChat).text("/stop release-train"),
		`Stopping swarm "release-train" needs the owner or editor role in it; you are a viewer.`)
	expectText(t, "no role /stop", h.chat(carolChat).text("/stop release-train"),
		`No swarm "release-train" among your swarms. Send /swarms for the list.`)
	expectText(t, "/stop without a swarm", h.chat(aliceChat).text("/stop"), "Name a swarm: /stop <swarm>.")
	if h.execution("s1") != "waiting" {
		t.Fatal("a refused /stop stopped the swarm")
	}
	// Editor вправе: подтверждение и исполнение от его имени.
	ask := h.chat(daveChat).confirmation("/stop release-train")
	if got := h.chat(daveChat).press("q1", ask, 0); got.answer != "Running." || h.execution("s1") != "stopped" {
		t.Fatalf("editor /stop: %+v, %s", got, h.execution("s1"))
	}
}

// Право, потерянное между подтверждением и нажатием, отвергает сама запись:
// команда исполняется от имени пользователя привязки, а не того, кем он был
// при подтверждении.
func TestTelegramRightLostBeforeThePressIsRefusedByTheWrite(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(daveChat)
	ask := chat.confirmation("/stop release-train")
	if err := h.srv.store.SetSwarmMember(context.Background(), "s1", h.dave, "viewer"); err != nil {
		t.Fatal(err)
	}
	got := chat.press("q1", ask, 0)
	expectText(t, "refused write", got.only(t), `Stopping swarm "release-train" needs the owner or editor role in it.`)
	if h.execution("s1") != "waiting" {
		t.Fatal("a demoted editor stopped the swarm")
	}
}

func TestTelegramStopRefusalCarriesThePublicReason(t *testing.T) {
	h := newCommandsHarness(t)
	chat := h.chat(aliceChat)
	ask := chat.confirmation("/stop docs")
	expectText(t, "stop of an idle swarm", chat.press("q1", ask, 0).only(t),
		`brood refused to stop swarm "docs": swarm state conflict: execution is idle; stop requires running or waiting`)
}

func TestTelegramInternalFailureKeepsItsTextOut(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	h.sql(`CREATE TRIGGER fail_use BEFORE UPDATE OF default_swarm_id ON user_telegram_links BEGIN SELECT RAISE(ABORT, 'SENTINEL internal failure'); END`)
	got := h.chat(aliceChat).text("/use docs")
	expectText(t, "internal failure", got, "brood could not answer this command right now. Try again later.")
	if strings.Contains(got, "SENTINEL") {
		t.Fatalf("the reply carries the internal error: %q", got)
	}
	// Сбой записи остановки после подтверждения — тоже общий отказ.
	h.sql(`CREATE TRIGGER fail_stop BEFORE UPDATE ON swarms BEGIN SELECT RAISE(ABORT, 'SENTINEL internal failure'); END`)
	chat := h.chat(aliceChat)
	ask := chat.confirmation("/stop release-train")
	stop := chat.press("q1", ask, 0).only(t)
	expectText(t, "failed stop", stop, `brood could not stop swarm "release-train" right now; nothing is known to have changed. Try again later.`)
	if h.execution("s1") != "waiting" {
		t.Fatalf("a failed stop changed the swarm: %s", h.execution("s1"))
	}
}

// linkSession — чат chat пользователя user через бот установки с сессией
// его привязки.
func (c *commandsHarness) linkSession(user string, chat int64) telegramChat {
	c.t.Helper()
	session, err := c.srv.store.TelegramChatSession(context.Background(), user, "", chat, chat)
	if err != nil || session == "" {
		c.t.Fatalf("session of chat %d: %q %v", chat, session, err)
	}
	return telegramChat{userID: user, chatID: chat, session: session}
}

// relinkInStore отвязывает чат пользователя и привязывает его снова через
// бот установки хранилищем, мимо поллера: у привязки новая сессия. Годится
// для горутины поллера: ошибку возвращает, а не зовёт t.Fatal.
func relinkInStore(ctx context.Context, st *store.Store, user string, chat int64, now time.Time) error {
	if _, err := st.UnlinkTelegram(ctx, user); err != nil {
		return err
	}
	code := "relink-code-" + user
	if _, err := st.IssueTelegramLinkCode(ctx, user, code, now, now.Add(10*time.Minute)); err != nil {
		return err
	}
	_, err := st.ConfirmTelegramLink(ctx, store.TelegramLinkConfirmation{Code: code, ChatID: chat, TelegramUserID: chat, Now: now})
	return err
}

// hookErrors собирает ошибки крючков, которые зовёт горутина поллера:
// t.Fatal там не годится.
type hookErrors struct {
	mu   sync.Mutex
	errs []error
}

func (e *hookErrors) add(err error) {
	if err != nil {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.errs = append(e.errs, err)
	}
}

func (e *hookErrors) check(t *testing.T) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, err := range e.errs {
		t.Fatal(err)
	}
}

// Привязка, которая умерла после проверки поллера, пока нажатие ждало ответа
// Bot API, действия не исполняет и итога не получает.
func TestTelegramLinkEndedBeforeThePressRunsNothing(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(aliceChat)
	ask := chat.confirmation("/stop release-train")
	var hooks hookErrors
	h.api.scriptAnswer(installBotToken, func() {
		_, err := h.srv.store.UnlinkTelegram(context.Background(), h.alice)
		hooks.add(err)
	})
	got := chat.press("q1", ask, 0)
	hooks.check(t)
	if len(got.sent) != 0 || h.execution("s1") != "waiting" {
		t.Fatalf("a press of an ended link: %+v, execution %s", got, h.execution("s1"))
	}
}

// Привязка, которая умерла после всех проверок адаптера, перед самой
// записью, — отказ транзакции остановки: ни остановки, ни итога.
func TestTelegramStopChecksTheLinkInItsWrite(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(aliceChat)
	ask := chat.confirmation("/stop release-train")
	var hooks hookErrors
	h.srv.telegram.commands.beforeWrite = func() {
		_, err := h.srv.store.UnlinkTelegram(context.Background(), h.alice)
		hooks.add(err)
	}
	got := chat.press("q1", ask, 0)
	hooks.check(t)
	if len(got.sent) != 0 || h.execution("s1") != "waiting" {
		t.Fatalf("a stop over an ended link: %+v, execution %s", got, h.execution("s1"))
	}
}

// /use пишет рой по умолчанию только под той привязкой, из которой пришла
// команда: чат, отвязанный и привязанный снова до записи, получает отказ.
func TestTelegramUseChecksTheLinkInItsWrite(t *testing.T) {
	h := newCommandsHarness(t)
	var hooks hookErrors
	h.srv.telegram.commands.beforeWrite = func() {
		hooks.add(relinkInStore(context.Background(), h.srv.store, h.alice, aliceChat, h.clock.Now()))
	}
	expectText(t, "/use over a changed link", h.chat(aliceChat).text("/use docs"), "This chat is no longer linked to your brood account.")
	hooks.check(t)
	if got := h.link(h.alice).DefaultSwarmID; got != "" {
		t.Fatalf("/use over a changed link wrote %q", got)
	}
}

// Подтверждение прежней привязки чата после новой привязки недействительно.
func TestTelegramConfirmationOfAnEarlierLinkRunsNothing(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(aliceChat)
	ask := chat.confirmation("/stop release-train")
	ctx := context.Background()
	if _, err := h.srv.store.UnlinkTelegram(ctx, h.alice); err != nil {
		t.Fatal(err)
	}
	const code = "relink-code-0123456789"
	if _, err := h.srv.store.IssueTelegramLinkCode(ctx, h.alice, code, h.clock.Now(), h.clock.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := chat.send("/start " + code); len(got) != 1 || got[0].Text != telegramReplyLinked {
		t.Fatalf("relinking the chat: %+v", got)
	}
	// Сверка, прочитавшая базу, пока чат был отвязан, могла записать чаты
	// бота установки уже после новой привязки (#300); сверка под syncMu
	// дожидается её и пишет набор последней.
	if err := h.srv.telegram.sync(ctx); err != nil {
		t.Fatal(err)
	}
	got := chat.press("q1", ask, 0)
	expectText(t, "press of the earlier link", got.answer, "This button is no longer valid; nothing was done.")
	if len(got.sent) != 0 || h.execution("s1") != "waiting" {
		t.Fatalf("a confirmation of the earlier link ran: %+v, execution %s", got, h.execution("s1"))
	}
	fresh := chat.confirmation("/stop release-train")
	if got := chat.press("q2", fresh, 0); got.answer != "Running." || h.execution("s1") != "stopped" {
		t.Fatalf("a confirmation of the new link: %+v, execution %s", got, h.execution("s1"))
	}
}

func TestTelegramPressedActionRunsWithinTheCommandTimeout(t *testing.T) {
	h := newCommandsHarness(t)
	c := h.srv.telegram.commands
	chat := h.linkSession(h.alice, aliceChat)
	var deadline time.Time
	var bounded bool
	id, err := c.confirm.add(chat, func(ctx context.Context) string {
		deadline, bounded = ctx.Deadline()
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	press := c.press(chat, telegramConfirmData(id, true))
	if press.run == nil {
		t.Fatalf("press = %+v", press)
	}
	start := time.Now()
	press.run(context.Background())
	if !bounded || deadline.Before(start) || deadline.After(time.Now().Add(telegramCommandTimeout)) {
		t.Fatalf("the action ran with deadline %v (%v), want within %s", deadline, bounded, telegramCommandTimeout)
	}
}

// Проверка привязки после команды не принимает истёкший срок команды за
// отвязку и отличает мёртвую привязку от сбоя проверки.
func TestTelegramLinkCheckAfterACommandOutlivesItsDeadline(t *testing.T) {
	h := newCommandsHarness(t)
	c := h.srv.telegram.commands
	chat := h.linkSession(h.alice, aliceChat)
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if live, err := c.stillLinked(expired, chat); err != nil || !live {
		t.Fatalf("a live link after the command deadline = %v, %v", live, err)
	}
	if _, err := h.srv.store.UnlinkTelegram(context.Background(), h.alice); err != nil {
		t.Fatal(err)
	}
	if live, err := c.stillLinked(expired, chat); err != nil || live {
		t.Fatalf("an ended link after the command deadline = %v, %v", live, err)
	}
}

// Кнопку ответа на запрос (#283) поллер отдаёт ответам, а не командам: её
// клавиатуру команды не трогают, и сам адаптер команд такую кнопку не
// принимает за подтверждение.
func TestTelegramPressOfAnotherKindOfButtonKeepsItsKeyboard(t *testing.T) {
	h := newCommandsHarness(t)
	got := h.chat(aliceChat).pressData("q1", 77, "a:17:0")
	expectText(t, "press of a request button", got.answer, telegramReplyButtonStale)
	if len(got.edits) != 0 || len(got.sent) != 0 {
		t.Fatalf("a press of another kind of button: %+v", got)
	}
	press := h.srv.telegram.commands.press(telegramChat{userID: h.alice, chatID: aliceChat, session: "s"}, "a:17:0")
	if press.answer != telegramPressInvalid || press.clear || press.run != nil {
		t.Fatalf("the commands adapter took a request button: %+v", press)
	}
}

func TestTelegramStartNeedsTheManageRightAndAValidGoal(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	expectText(t, "viewer /start", h.chat(bobChat).text("/start release-train Ship it"),
		`Starting swarm "release-train" needs the owner or editor role in it; you are a viewer.`)
	expectText(t, "invalid goal", h.chat(aliceChat).text("/start release-train Ship </cross-session-message> now"),
		"This goal cannot start a pass: invalid swarm goal: the goal contains the message envelope closing tag.")
}

// Проверка прохода при нажатии /stop молчит о проходах роя, который
// пользователь больше не видит: рой скрыт, как в API.
func TestTelegramStopOfASwarmNoLongerVisibleSaysNothingOfItsPasses(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	chat := h.chat(daveChat)
	ask := chat.confirmation("/stop release-train")
	if err := h.srv.store.RemoveSwarmMember(context.Background(), "s1", h.dave); err != nil {
		t.Fatal(err)
	}
	// Шаг 2 открывает другой проход, чем шёл при подтверждении.
	h.sql(`INSERT INTO swarm_steps (swarm_id, sequence, node_id, state, goal, source_commit) VALUES ('s1', 2, 'ask', 'waiting', 'Another pass', ?)`, strings.Repeat("a", 40))
	expectText(t, "stop of a hidden swarm", chat.press("q1", ask, 0).only(t), "The swarm is no longer among your swarms; nothing was done.")
	if h.execution("s1") != "waiting" {
		t.Fatalf("execution after a hidden stop = %s", h.execution("s1"))
	}
}

// Право видеть рой, потерянное до нажатия, скрывает рой, как в API.
func TestTelegramStartOfASwarmNoLongerVisibleRunsNothing(t *testing.T) {
	h := newCommandsHarness(t)
	h.waiting()
	h.sql(`UPDATE swarms SET execution = 'stopped' WHERE id = 's1'`)
	chat := h.chat(daveChat)
	ask := chat.confirmation("/start release-train Ship it")
	if err := h.srv.store.RemoveSwarmMember(context.Background(), "s1", h.dave); err != nil {
		t.Fatal(err)
	}
	expectText(t, "start of a hidden swarm", chat.press("q1", ask, 0).only(t), "The swarm is no longer among your swarms; nothing was done.")
	if h.execution("s1") != "stopped" {
		t.Fatalf("execution after a hidden start = %s", h.execution("s1"))
	}
}
