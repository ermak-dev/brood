package server

// FEATURE: Answers to operator requests from Telegram (#283, epic #278).
// SPEC: a request notification reaches an owner or editor with answer
// controls (option buttons, Yes and No, a reply or /answer for text) and a
// viewer without them; options Telegram cannot show as buttons leave the
// notification without buttons, not unsent. A press, a reply to a text
// request message or /answer is recorded by the same store method as
// POST /api/swarms/{id}/requests/{request}/answer, as the user of the linked
// chat, with the telegram session as the live session and the rights checked
// in that transaction: a viewer, or an editor demoted after the keyboard was
// sent, is refused and nothing is recorded, and a chat whose link or
// session is gone gets no reply at all, even when the session goes between
// the link check and the answer. A recorded answer to an actor is delivered
// to it, and the server stops only after that delivery. A press Telegram
// repeats is the same answer again, recorded once. A press on an answered,
// cancelled or stale request is answered with the request's actual state and
// records nothing. An open request keeps its buttons. After an answer, from
// Telegram or the panel, the request messages of those who read the swarm
// show one status button that names who answered and when, through the bot
// each was sent by, and the message of one who no longer reads it only loses
// its buttons; a message Telegram no longer lets the bot edit is not an
// error. A request answered while its notification was in flight gets its
// status right after the send, once per message. /answer without a pending
// text request is a short refusal with its reason, and plain text that
// answers nothing gets the way to answer.
// IMPLEMENTATION: telegram_answers.go, telegram_buttons.go,
// telegramSender (telegram_sender.go), answerRequest
// (swarm_requests_api.go), the store reads of telegram_answers.go.
// STATUS: DONE

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/store"
)

// answerGraph — оператор выбирает Ship или Hold, и проход кончается.
const answerGraph = `apiVersion: brood.dev/v1alpha1
name: approve
start: ask
exit: done
nodes:
  ask:
    type: human
    question: Ship build 42?
    options:
      - {label: Ship, outcome: success}
      - {label: Hold, outcome: fail}
  done: {type: exit}
edges:
  - {from: ask, to: done}
goalGates: [done]
`

// Личные чаты участников роя s1, привязанные через бот установки.
const (
	ownerChat  = 501
	editorChat = 502
	viewerChat = 503
)

// answersHarness — сервер с ботом установки; в рое s1 «release-train»
// проекта p alice — owner, bob — editor, carol — viewer, все с чатами, и
// живой актор worker, который задаёт вопросы.
type answersHarness struct {
	*telegramHarness
	alice, bob, carol string
	worker            store.ManagedActor
	presses           atomic.Int64
}

func newAnswersHarness(t *testing.T) *answersHarness {
	t.Helper()
	a := &answersHarness{}
	a.telegramHarness = startTelegramHarness(t, newTestBotAPI(t), "", installBotToken, func(ctx context.Context, st *store.Store) {
		a.alice = seedMirror(t, ctx, st)
		a.bob = createTelegramUser(t, ctx, st, "bob")
		a.carol = createTelegramUser(t, ctx, st, "carol")
		for user, role := range map[string]string{a.bob: "editor", a.carol: "viewer"} {
			if err := st.AddMember(ctx, "p", user); err != nil {
				t.Fatal(err)
			}
			if err := st.SetSwarmMember(ctx, "s1", user, role); err != nil {
				t.Fatal(err)
			}
		}
		linkInStore(t, ctx, st, a.bob, editorChat)
		linkInStore(t, ctx, st, a.carol, viewerChat)
		a.worker = seedWorker(t, ctx, st)
	})
	// Reconcile managed-акторов без PVE записал бы worker ошибкой: проверки
	// держат его живым сами.
	a.srv.managed.stopReconcile()
	a.sql(`UPDATE managed_actors SET phase = 'running', desired = 'running', stage = 'done', operation = 'start', error = '' WHERE uid = ?`, a.worker.UID)
	a.worker.Phase = "running"
	telegramEventually(t, "the installation bot polls", func() bool { return len(a.api.polls(installBotToken)) > 0 })
	return a
}

// seedWorker заводит managed-актора worker роя s1; живым его делает
// newAnswersHarness.
func seedWorker(t *testing.T, ctx context.Context, st *store.Store) store.ManagedActor {
	t.Helper()
	pool, err := st.CreateManagedPool(ctx, store.ManagedPool{UID: "pool", Name: "pve", Connection: "configured", Source: store.ManagedSourceSnapshot{ClusterIdentity: "cluster", Endpoint: "https://pve.invalid:8006", Node: "node", ResourcePool: "brood", Storage: "local", Network: "vmbr0"}, Labels: map[string]string{"provider": "proxmox"}, MaxActors: 8, Resources: store.ManagedResources{CPUMillicores: 8000, MemoryBytes: 8192, DiskBytes: 16384}, Template: store.ManagedTemplateSnapshot{Profile: "claude-proxmox", URI: "local:template.tar.zst", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, State: "enabled"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := st.ReserveManagedActor(ctx, store.ManagedActor{UID: "worker-uid", Name: "worker", SwarmID: "s1", AtespaceUID: "native-s1", PoolUID: pool.UID, Resources: store.ManagedResources{CPUMillicores: 1000, MemoryBytes: 1024, DiskBytes: 2048}, CTID: 101, OwnerNonce: "owner-worker", OperationID: "create-worker"})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

// openNode запускает узловой запрос answerGraph и возвращает его.
func (a *answersHarness) openNode() store.SwarmRequest {
	a.t.Helper()
	ctx := context.Background()
	if err := insertSwarmGraph(ctx, a.testServer, "s1", []byte(answerGraph), "waiting"); err != nil {
		a.t.Fatal(err)
	}
	if _, err := a.srv.store.AppendPipelineVisit(ctx, "s1", "s1", store.PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		a.t.Fatal(err)
	}
	for _, req := range a.requests() {
		if req.Origin == "node" && req.State == "open" {
			return req
		}
	}
	a.t.Fatal("no open node request")
	return store.SwarmRequest{}
}

// ask открывает вопрос актора worker вида kind.
func (a *answersHarness) ask(kind string, options ...store.RequestOption) store.SwarmRequest {
	a.t.Helper()
	req, err := a.srv.store.OpenActorRequest(context.Background(), store.ActorRequestInput{SwarmID: "s1", ActorUID: a.worker.UID, ActorIncarnation: a.worker.Incarnation,
		Kind: kind, Question: "Which database?", Options: options})
	if err != nil {
		a.t.Fatal(err)
	}
	return req
}

func (a *answersHarness) requests() []store.SwarmRequest {
	a.t.Helper()
	requests, err := a.srv.store.SwarmRequests(context.Background(), "s1")
	if err != nil {
		a.t.Fatal(err)
	}
	return requests
}

func (a *answersHarness) request(id string) store.SwarmRequest {
	a.t.Helper()
	req, err := a.srv.store.GetSwarmRequest(context.Background(), "s1", id)
	if err != nil {
		a.t.Fatal(err)
	}
	return req
}

// requestMessage — отправленное уведомление пользователя user, открывшее
// запрос: номер в outbox, номер сообщения и само сообщение.
type requestMessage struct {
	id, message int64
	sent        fakeSent
}

// sentRequest ждёт, пока уведомление запроса req уйдёт пользователю user
// через бот установки.
func (a *answersHarness) sentRequest(user string, req store.SwarmRequest) requestMessage {
	a.t.Helper()
	return a.sentRequestVia(installBotToken, user, req)
}

// sentRequestVia ждёт, пока уведомление запроса req уйдёт пользователю user
// через бот token.
func (a *answersHarness) sentRequestVia(token, user string, req store.SwarmRequest) requestMessage {
	a.t.Helper()
	query := `SELECT coalesce(max(id), 0) FROM telegram_outbox WHERE user_id = ? AND ref = ? AND kind IN ('request_opened', 'actor_question') AND state = 'sent'`
	ref := "swarm/s1/request/" + req.ID
	telegramEventually(a.t, "the request notification is sent", func() bool { return a.column(query, user, ref) != "0" })
	id, err := strconv.ParseInt(a.column(query, user, ref), 10, 64)
	if err != nil {
		a.t.Fatal(err)
	}
	message, err := strconv.ParseInt(a.column(`SELECT telegram_message_id FROM telegram_outbox WHERE id = ?`, id), 10, 64)
	if err != nil {
		a.t.Fatal(err)
	}
	sent := a.api.sent(token)
	if message < 1 || int(message) > len(sent) {
		a.t.Fatalf("message %d of notification %d is not in the chat", message, id)
	}
	return requestMessage{id: id, message: message, sent: sent[message-1]}
}

// press нажимает в чате chat кнопку data сообщения message и возвращает
// ответ бота на нажатие.
func (a *answersHarness) press(chat, message int64, data string) string {
	a.t.Helper()
	query := "cb-" + strconv.FormatInt(a.presses.Add(1), 10)
	a.deliver(installBotToken, callbackPress(query, chat, message, data))
	for _, answer := range a.api.callbackAnswers(installBotToken) {
		if answer.ID == query {
			return answer.Text
		}
	}
	a.t.Fatalf("press %s %q got no answer", query, data)
	return ""
}

// say отправляет текст из чата chat и возвращает ответ бота.
func (a *answersHarness) say(chat int64, text string) string {
	a.t.Helper()
	return a.send(chat, privateMessage(chat, "", text))
}

// send доставляет сообщение update из чата chat и возвращает ответ бота.
func (a *answersHarness) send(chat int64, update map[string]any) string {
	a.t.Helper()
	before := len(a.api.sentTo(installBotToken, chat))
	a.deliver(installBotToken, update)
	replies := a.repliesSince(chat, before)
	if len(replies) != 1 {
		a.t.Fatalf("%v got replies %q, want one", update["message"], replies)
	}
	return replies[0]
}

// repliesSince — ответы бота в чат chat после первых before его сообщений.
// Уведомления, которые отправитель шлёт в тот же чат, ответом не считаются.
func (a *answersHarness) repliesSince(chat int64, before int) []string {
	var replies []string
	for _, sent := range a.api.sentTo(installBotToken, chat)[before:] {
		if !strings.HasPrefix(sent, `Swarm "`) {
			replies = append(replies, sent)
		}
	}
	return replies
}

// privateReply — текст из личного чата chat ответом (reply) на сообщение
// бота message.
func privateReply(chat, message int64, text string) map[string]any {
	update := privateMessage(chat, "", text)
	update["message"].(map[string]any)["reply_to_message"] = map[string]any{"message_id": message,
		"from": map[string]any{"id": 710000009, "is_bot": true, "first_name": "Bot"},
		"chat": map[string]any{"id": chat, "type": "private"}, "date": 1763000000, "text": "request"}
	return update
}

// pressAnswered: на нажатие query бот ответил answerCallbackQuery.
func (a *answersHarness) pressAnswered(query string) bool {
	for _, answer := range a.api.callbackAnswers(installBotToken) {
		if answer.ID == query {
			return true
		}
	}
	return false
}

// revokeSession удаляет сессию telegram пользователя user мимо хранилища,
// как её отзыв: привязка падает в ошибку.
func (a *answersHarness) revokeSession(user string) {
	a.t.Helper()
	a.sql(`DELETE FROM sessions WHERE user_id = ? AND kind = 'telegram'`, user)
}

// callbackPress — нажатие кнопки сообщения бота в личном чате.
func callbackPress(id string, chat, message int64, data string) map[string]any {
	person := map[string]any{"id": chat, "is_bot": false, "first_name": "Person"}
	return map[string]any{"callback_query": map[string]any{"id": id, "from": person, "chat_instance": "1", "data": data,
		"message": map[string]any{"message_id": message, "from": map[string]any{"id": 710000009, "is_bot": true, "first_name": "Bot"},
			"chat": map[string]any{"id": chat, "type": "private"}, "date": 1763000000, "text": "request"}}}
}

// editsOf — правки клавиатур сообщения message чата chat ботом установки.
func (a *answersHarness) editsOf(chat, message int64) []fakeEdit {
	return a.editsVia(installBotToken, chat, message)
}

// editsVia — правки клавиатур сообщения message чата chat ботом token.
func (a *answersHarness) editsVia(token string, chat, message int64) []fakeEdit {
	var out []fakeEdit
	for _, e := range a.api.edits(token) {
		if e.ChatID == chat && e.MessageID == message {
			out = append(out, e)
		}
	}
	return out
}

// statusShown ждёт, пока сообщение m чата chat бота установки получит
// кнопку состояния с текстом, который начинается с want.
func (a *answersHarness) statusShown(chat int64, m requestMessage, want string) {
	a.t.Helper()
	a.statusShownVia(installBotToken, chat, m, want)
}

// statusShownVia — statusShown сообщения бота token.
func (a *answersHarness) statusShownVia(token string, chat int64, m requestMessage, want string) {
	a.t.Helper()
	telegramEventually(a.t, "the message shows the state of the request", func() bool {
		for _, e := range a.editsVia(token, chat, m.message) {
			if e.Markup != nil && len(e.Markup.InlineKeyboard) == 1 && len(e.Markup.InlineKeyboard[0]) == 1 {
				b := e.Markup.InlineKeyboard[0][0]
				if strings.HasPrefix(b.Text, want) && b.CallbackData == telegramStatusData(m.id) {
					return true
				}
			}
		}
		return false
	})
}

// answeredBy проверяет, что запрос id отвечен user с исходом и текстом.
func (a *answersHarness) answeredBy(id, user, outcome, response string) store.SwarmRequest {
	a.t.Helper()
	req := a.request(id)
	got := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if req.State != "answered" || req.AnsweredBy != user || got(req.Outcome) != outcome || got(req.Response) != response {
		a.t.Fatalf("request %s: %s by %s, outcome %q, response %q; want answered by %s, %q, %q", id, req.State, req.AnsweredBy, got(req.Outcome), got(req.Response), user, outcome, response)
	}
	return req
}

// answeredEvents — число уведомлений request_answered о запросе id.
func (a *answersHarness) answeredEvents(id string) string {
	return a.column(`SELECT count(*) FROM telegram_outbox WHERE kind = 'request_answered' AND ref = ?`, "swarm/s1/request/"+id)
}

func TestManagersGetAnswerButtonsAndAViewerDoesNot(t *testing.T) {
	a := newAnswersHarness(t)
	req := a.openNode()
	for _, user := range []string{a.alice, a.bob} {
		m := a.sentRequest(user, req)
		if m.sent.Markup == nil || len(m.sent.Markup.InlineKeyboard) != 2 ||
			m.sent.Markup.InlineKeyboard[0][0].Text != "Ship" || m.sent.Markup.InlineKeyboard[0][0].CallbackData != telegramAnswerData(m.id, 0) ||
			m.sent.Markup.InlineKeyboard[1][0].Text != "Hold" || m.sent.Markup.InlineKeyboard[1][0].CallbackData != telegramAnswerData(m.id, 1) {
			t.Fatalf("manager's request message: %+v", m.sent)
		}
	}
	viewer := a.sentRequest(a.carol, req)
	if viewer.sent.Markup != nil || strings.Contains(viewer.sent.Text, "button") {
		t.Fatalf("viewer's request message offers controls: %+v", viewer.sent)
	}
	question := a.ask("text")
	if m := a.sentRequest(a.carol, question); m.sent.Markup != nil || strings.Contains(m.sent.Text, "/answer") {
		t.Fatalf("viewer's text question offers /answer: %+v", m.sent)
	}
	if m := a.sentRequest(a.alice, question); m.sent.Markup != nil || !strings.Contains(m.sent.Text, "/answer "+strconv.FormatInt(m.id, 10)+" ") {
		t.Fatalf("owner's text question: %+v", m.sent)
	}
}

// Правка после отправки трогает только закрытый запрос: сообщение открытого
// сохраняет кнопки ответа.
func TestOpenRequestMessageKeepsItsAnswerButtons(t *testing.T) {
	a := newAnswersHarness(t)
	req := a.openNode()
	mine, editor := a.sentRequest(a.alice, req), a.sentRequest(a.bob, req)
	// Отправитель сверяет запрос сразу после записи sent; правка, будь она,
	// пришла бы за это время.
	time.Sleep(200 * time.Millisecond)
	for chat, m := range map[int64]requestMessage{ownerChat: mine, editorChat: editor} {
		if edits := a.editsOf(chat, m.message); len(edits) != 0 {
			t.Fatalf("the message of an open request in chat %d was edited: %+v", chat, edits)
		}
	}
	// Ответ правит те же сообщения: проверка выше видела бы правку.
	a.press(ownerChat, mine.message, telegramAnswerData(mine.id, 0))
	state := telegramRequestState(a.answeredBy(req.ID, a.alice, "success", ""), "alice")
	a.statusShown(ownerChat, mine, state)
	a.statusShown(editorChat, editor, state)
}

// Подпись варианта, которую граф и хранилище принимают, а Telegram кнопкой не
// покажет, не отнимает уведомления: оно уходит без кнопок и говорит ответить
// в brood.
func TestChoiceTelegramCannotShowAsButtonsIsSentWithoutThem(t *testing.T) {
	a := newAnswersHarness(t)
	choice := a.ask("choice", store.RequestOption{Label: " ", Outcome: "blank"}, store.RequestOption{Label: "postgres", Outcome: "postgres"})
	m := a.sentRequest(a.alice, choice)
	if m.sent.Markup != nil || !strings.Contains(m.sent.Text, "in brood") || strings.Contains(m.sent.Text, "button below") {
		t.Fatalf("owner's choice with a blank option: %+v", m.sent)
	}
}

// Reply на сообщение текстового запроса — ответ на него тем же путём, что
// /answer с номером его уведомления; текст, который ни на что не отвечает,
// получает подсказку, как ответить.
func TestReplyToATextRequestMessageAnswersIt(t *testing.T) {
	a := newAnswersHarness(t)
	node := a.openNode()
	nodeMessage := a.sentRequest(a.alice, node)
	text := a.ask("text")
	mine, viewer := a.sentRequest(a.alice, text), a.sentRequest(a.carol, text)
	for _, tc := range []struct {
		chat   int64
		update map[string]any
		want   string
	}{
		{ownerChat, privateMessage(ownerChat, "", "use sqlite"), telegramReplyTextUsage},
		{ownerChat, privateReply(ownerChat, 999999, "use sqlite"), telegramReplyTextUsage},
		{ownerChat, privateReply(ownerChat, mine.message, "   "), telegramReplyTextUsage},
		// Номер сообщения чужого чата в этом чате называет не его запрос.
		{editorChat, privateReply(editorChat, mine.message, "use sqlite"), telegramReplyTextUsage},
		{ownerChat, privateReply(ownerChat, nodeMessage.message, "ship it"), telegramReplyNotText(nodeMessage.id)},
		{viewerChat, privateReply(viewerChat, viewer.message, "mysql"), telegramReplyDenied},
	} {
		if got := a.send(tc.chat, tc.update); got != tc.want {
			t.Errorf("%v: reply %q, want %q", tc.update["message"], got, tc.want)
		}
	}
	if got := a.request(text.ID); got.State != "open" || a.answeredEvents(text.ID) != "0" {
		t.Fatalf("refused replies changed the request: %+v", got)
	}
	reply := a.send(ownerChat, privateReply(ownerChat, mine.message, "  use sqlite\nwith WAL  "))
	answered := a.answeredBy(text.ID, a.alice, "", "use sqlite\nwith WAL")
	if want := telegramRequestState(answered, "alice"); reply != want {
		t.Fatalf("reply answer = %q, want %q", reply, want)
	}
	a.statusShown(ownerChat, mine, "Answered by alice at ")
}

// Чат без живой привязки не отвечает на запросы: снятая привязка и отозванная
// сессия telegram — ни ответа на нажатие, ни сообщения, ни записи.
func TestChatWithoutALiveLinkCannotAnswer(t *testing.T) {
	ctx := context.Background()
	a := newAnswersHarness(t)
	node := a.openNode()
	text := a.ask("text")
	chats := map[int64]struct{ node, text requestMessage }{
		ownerChat:  {a.sentRequest(a.alice, node), a.sentRequest(a.alice, text)},
		editorChat: {a.sentRequest(a.bob, node), a.sentRequest(a.bob, text)},
	}
	if _, err := a.srv.store.UnlinkTelegram(ctx, a.bob); err != nil {
		t.Fatal(err)
	}
	a.revokeSession(a.alice)
	for chat, m := range chats {
		before := len(a.api.sentTo(installBotToken, chat))
		query := "cb-dead-" + strconv.FormatInt(chat, 10)
		a.deliver(installBotToken, callbackPress(query, chat, m.node.message, telegramAnswerData(m.node.id, 0)))
		a.deliver(installBotToken, privateMessage(chat, "", "/answer "+strconv.FormatInt(m.text.id, 10)+" use sqlite"))
		a.deliver(installBotToken, privateReply(chat, m.text.message, "use sqlite"))
		if a.pressAnswered(query) {
			t.Errorf("a press from chat %d without a live link was answered", chat)
		}
		if replies := a.repliesSince(chat, before); len(replies) != 0 {
			t.Errorf("chat %d without a live link got replies %q", chat, replies)
		}
	}
	for _, req := range []store.SwarmRequest{node, text} {
		if got := a.request(req.ID); got.State != "open" || got.AnsweredBy != "" || a.answeredEvents(req.ID) != "0" {
			t.Fatalf("a chat without a live link answered request %s: %+v", req.ID, got)
		}
	}
}

// Сессию telegram проверяет транзакция ответа, а не только проверка привязки
// до неё: сессия, отозванная после проверки, ответа не записывает.
func TestAnswerChecksTheTelegramSessionInItsTransaction(t *testing.T) {
	ctx := context.Background()
	a := newAnswersHarness(t)
	req := a.openNode()
	p := a.poller("")
	if p == nil {
		t.Fatal("no poller of the installation bot")
	}
	if owner, linked, err := p.chatOwner(ctx, ownerChat, ownerChat); err != nil || !linked || owner != a.alice {
		t.Fatalf("chat owner = %q, %t, %v; want alice, linked", owner, linked, err)
	}
	a.revokeSession(a.alice)
	got := p.answer(ctx, a.alice, ownerChat, ownerChat, store.SwarmAnswer{SwarmID: "s1", RequestID: req.ID, UserID: a.alice, Outcome: "success"})
	if got != telegramReplyDenied {
		t.Fatalf("answer after the session was revoked = %q, want %q", got, telegramReplyDenied)
	}
	if got := a.request(req.ID); got.State != "open" || got.AnsweredBy != "" || a.answeredEvents(req.ID) != "0" {
		t.Fatalf("an answer without the telegram session was recorded: %+v", got)
	}
}

// Ответ актору из Telegram уходит спросившему: доставку начинает ответ, её
// исход записан и зеркалится answer_delivery. У worker сборки нет relay, и
// доставка не удаётся, но попытка записана.
func TestTelegramAnswerIsDeliveredToTheAskingActor(t *testing.T) {
	a := newAnswersHarness(t)
	question := a.ask("confirm")
	m := a.sentRequest(a.alice, question)
	a.press(ownerChat, m.message, telegramAnswerData(m.id, 0))
	a.answeredBy(question.ID, a.alice, "", "yes")
	telegramEventually(t, "the answer delivery is tried", func() bool { return a.request(question.ID).Delivery != "pending" })
	if got := a.column(`SELECT count(*) FROM telegram_outbox WHERE kind = 'answer_delivery' AND ref = ?`, "swarm/s1/request/"+question.ID); got == "0" {
		t.Fatal("the delivery outcome queued no answer_delivery notification")
	}
}

// Server.Close ждёт доставку ответа из Telegram: её начал поллер, и исход
// записан до закрытия базы.
func TestServerCloseWaitsForTheDeliveryOfATelegramAnswer(t *testing.T) {
	a := newAnswersHarness(t)
	question := a.ask("confirm")
	m := a.sentRequest(a.alice, question)
	// Доставки одного ответа сериализует замок доставки: тест держит его, и
	// доставка ответа ждёт.
	unlock := a.srv.deliveries.lock(question.ID)
	var once sync.Once
	release := func() { once.Do(unlock) }
	defer release()
	a.press(ownerChat, m.message, telegramAnswerData(m.id, 0))
	a.answeredBy(question.ID, a.alice, "", "yes")
	closed := a.closeServer()
	select {
	case <-closed:
		t.Fatal("Close returned while the delivery of a Telegram answer was running")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	waitClosed(t, "Close returns", closed)
	if got := a.column(`SELECT delivery FROM swarm_requests WHERE id = ?`, question.ID); got == "pending" {
		t.Fatal("Close returned before the delivery outcome was recorded")
	}
}

// Сообщение правит бот, которым оно отправлено: у привязки через свой бот —
// он, а не бот установки.
func TestPanelAnswerEditsAMessageThroughTheBotThatSentIt(t *testing.T) {
	a := newAnswersHarness(t)
	client := a.signIn("alice")
	a.setBot(client, a.alice, aliceBotToken)
	a.linkChat(client, aliceBotToken, ownerChat, "alice")
	req := a.openNode()
	mine := a.sentRequestVia(aliceBotToken, a.alice, req)
	if mine.sent.Markup == nil {
		t.Fatalf("owner's request message through the own bot has no buttons: %+v", mine.sent)
	}
	resp, data, _ := a.do(client, "POST", "/api/swarms/s1/requests/"+req.ID+"/answer", a.csrf(client), map[string]any{"outcome": "success"})
	expectStatus(t, "panel answer", resp, data, http.StatusOK)
	answered := a.answeredBy(req.ID, a.alice, "success", "")
	a.statusShownVia(aliceBotToken, ownerChat, mine, telegramRequestState(answered, "alice"))
	if edits := a.editsOf(ownerChat, mine.message); len(edits) != 0 {
		t.Fatalf("the installation bot edited a message of the own bot: %+v", edits)
	}
}

// После ответа сообщение запроса показывает состояние каждому, кто читает рой
// сейчас, и тому, кого понизили после отправки кнопок. Тот, кто рой больше не
// читает, состояния не видит: у его сообщения снимаются кнопки ответа.
func TestAnswerEditsTheMessagesOfReadersAndStripsTheButtonsOfOthers(t *testing.T) {
	ctx := context.Background()
	a := newAnswersHarness(t)
	const formerChat = 504
	dave := createTelegramUser(t, ctx, a.srv.store, "dave")
	if err := a.srv.store.AddMember(ctx, "p", dave); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.store.SetSwarmMember(ctx, "s1", dave, "editor"); err != nil {
		t.Fatal(err)
	}
	linkInStore(t, ctx, a.srv.store, dave, formerChat)
	req := a.openNode()
	mine, demoted, viewer, former := a.sentRequest(a.alice, req), a.sentRequest(a.bob, req), a.sentRequest(a.carol, req), a.sentRequest(dave, req)
	if demoted.sent.Markup == nil || former.sent.Markup == nil {
		t.Fatalf("editors got no buttons: %+v, %+v", demoted.sent, former.sent)
	}
	if err := a.srv.store.SetSwarmMember(ctx, "s1", a.bob, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.store.RemoveSwarmMember(ctx, "s1", dave); err != nil {
		t.Fatal(err)
	}
	a.press(ownerChat, mine.message, telegramAnswerData(mine.id, 0))
	state := telegramRequestState(a.answeredBy(req.ID, a.alice, "success", ""), "alice")
	a.statusShown(ownerChat, mine, state)
	a.statusShown(editorChat, demoted, state)
	a.statusShown(viewerChat, viewer, state)
	telegramEventually(t, "the buttons of a former member are removed", func() bool { return len(a.editsOf(formerChat, former.message)) > 0 })
	for _, e := range a.editsOf(formerChat, former.message) {
		if e.Markup != nil {
			t.Fatalf("a former member's message got a keyboard: %+v", e.Markup)
		}
	}
}

func TestOwnerAnswersANodeRequestWithAButton(t *testing.T) {
	a := newAnswersHarness(t)
	req := a.openNode()
	mine, editor := a.sentRequest(a.alice, req), a.sentRequest(a.bob, req)
	viewer := a.sentRequest(a.carol, req)
	reply := a.press(ownerChat, mine.message, telegramAnswerData(mine.id, 1))
	answered := a.answeredBy(req.ID, a.alice, "fail", "")
	if !strings.HasPrefix(reply, "Answered by alice at ") || !strings.HasSuffix(reply, ": Hold.") {
		t.Fatalf("answer to the press = %q", reply)
	}
	if w, err := a.srv.store.GetSwarm(context.Background(), "s1"); err != nil || w.Execution == "waiting" {
		t.Fatalf("the graph did not move on the answer: %+v %v", w, err)
	}
	// Кнопки ответа уступают кнопке состояния — кто и когда ответил; её
	// получает и сообщение viewer, который рой читает.
	state := telegramRequestState(answered, "alice")
	a.statusShown(ownerChat, mine, state)
	a.statusShown(editorChat, editor, state)
	a.statusShown(viewerChat, viewer, state)
	// Кнопка состояния показывает состояние и ничего не пишет.
	if got := a.press(ownerChat, mine.message, telegramStatusData(mine.id)); got != state {
		t.Fatalf("status press = %q, want %q", got, state)
	}
	// Тот, кто больше не читает рой, состояния его запроса не узнаёт.
	if err := a.srv.store.RemoveSwarmMember(context.Background(), "s1", a.bob); err != nil {
		t.Fatal(err)
	}
	if got := a.press(editorChat, editor.message, telegramStatusData(editor.id)); got != telegramReplyHidden {
		t.Fatalf("status press of a former member = %q", got)
	}
}

func TestActorQuestionsOfEachKindAreAnsweredFromTelegram(t *testing.T) {
	a := newAnswersHarness(t)
	choice := a.ask("choice", store.RequestOption{Label: "sqlite", Outcome: "sqlite"}, store.RequestOption{Label: "postgres", Outcome: "postgres"})
	m := a.sentRequest(a.alice, choice)
	if m.sent.Markup == nil || len(m.sent.Markup.InlineKeyboard) != 2 {
		t.Fatalf("choice message: %+v", m.sent)
	}
	if reply := a.press(ownerChat, m.message, telegramAnswerData(m.id, 1)); !strings.HasSuffix(reply, ": postgres.") {
		t.Fatalf("choice press = %q", reply)
	}
	a.answeredBy(choice.ID, a.alice, "", "postgres")

	confirm := a.ask("confirm")
	m = a.sentRequest(a.bob, confirm)
	if m.sent.Markup == nil || len(m.sent.Markup.InlineKeyboard) != 1 || len(m.sent.Markup.InlineKeyboard[0]) != 2 {
		t.Fatalf("confirm message: %+v", m.sent)
	}
	if reply := a.press(editorChat, m.message, telegramAnswerData(m.id, 1)); !strings.HasPrefix(reply, "Answered by bob at ") || !strings.HasSuffix(reply, ": no.") {
		t.Fatalf("confirm press = %q", reply)
	}
	a.answeredBy(confirm.ID, a.bob, "", "no")

	text := a.ask("text")
	m = a.sentRequest(a.alice, text)
	reply := a.say(ownerChat, "/answer "+strconv.FormatInt(m.id, 10)+" use sqlite\nwith WAL")
	answered := a.answeredBy(text.ID, a.alice, "", "use sqlite\nwith WAL")
	if want := telegramRequestState(answered, "alice"); reply != want {
		t.Fatalf("/answer reply = %q, want %q", reply, want)
	}
	a.statusShown(ownerChat, m, "Answered by alice at ")
}

// Telegram повторяет нажатие (порция после перезапуска поллера): тот же
// автор с тем же исходом — тот же ответ, и записан он один раз.
func TestRepeatedPressIsTheSameAnswerRecordedOnce(t *testing.T) {
	a := newAnswersHarness(t)
	req := a.openNode()
	m := a.sentRequest(a.alice, req)
	first := a.press(ownerChat, m.message, telegramAnswerData(m.id, 0))
	answered := a.answeredBy(req.ID, a.alice, "success", "")
	again := a.press(ownerChat, m.message, telegramAnswerData(m.id, 0))
	if first != again || !strings.HasSuffix(again, ": Ship.") {
		t.Fatalf("repeated press = %q, first %q", again, first)
	}
	if repeated := a.answeredBy(req.ID, a.alice, "success", ""); *repeated.AnsweredAt != *answered.AnsweredAt {
		t.Fatalf("the repeat rewrote the answer: %s, first %s", *repeated.AnsweredAt, *answered.AnsweredAt)
	}
	if got := a.answeredEvents(req.ID); got != "3" {
		t.Fatalf("%s request_answered notifications, want one per reader", got)
	}
	// Другой вариант после ответа — конфликт: состояние запроса, без записи.
	if other := a.press(ownerChat, m.message, telegramAnswerData(m.id, 1)); other != first {
		t.Fatalf("another option after the answer = %q, want the state %q", other, first)
	}
	a.answeredBy(req.ID, a.alice, "success", "")
}

func TestAnswerWithoutTheRightIsRefusedAndNothingIsRecorded(t *testing.T) {
	ctx := context.Background()
	a := newAnswersHarness(t)
	req := a.openNode()
	viewer, editor := a.sentRequest(a.carol, req), a.sentRequest(a.bob, req)
	// Viewer кнопок не получил, но данные кнопки подделать можно: права
	// проверяет транзакция ответа.
	if reply := a.press(viewerChat, viewer.message, telegramAnswerData(viewer.id, 0)); reply != telegramReplyDenied {
		t.Fatalf("viewer's press = %q", reply)
	}
	// Кнопки отправлены редактору, право снято после: решает момент ответа.
	if err := a.srv.store.SetSwarmMember(ctx, "s1", a.bob, "viewer"); err != nil {
		t.Fatal(err)
	}
	if reply := a.press(editorChat, editor.message, telegramAnswerData(editor.id, 0)); reply != telegramReplyDenied {
		t.Fatalf("demoted editor's press = %q", reply)
	}
	// Чужое уведомление чат не находит.
	if reply := a.press(viewerChat, viewer.message, telegramAnswerData(editor.id, 0)); reply != telegramReplyButtonStale {
		t.Fatalf("press on another user's notification = %q", reply)
	}
	if got := a.request(req.ID); got.State != "open" || got.AnsweredBy != "" {
		t.Fatalf("refused answers changed the request: %+v", got)
	}
	if got := a.answeredEvents(req.ID); got != "0" {
		t.Fatalf("refused answers queued %s request_answered notifications", got)
	}
}

func TestPressOnAClosedOrStaleRequestShowsItsState(t *testing.T) {
	ctx := context.Background()
	a := newAnswersHarness(t)
	allow := func(context.Context) bool { return true }

	node := a.openNode()
	editor := a.sentRequest(a.bob, node)
	if _, err := a.srv.store.AnswerSwarmRequest(ctx, store.SwarmAnswer{SwarmID: "s1", RequestID: node.ID, UserID: a.alice, Outcome: "success"}, allow); err != nil {
		t.Fatal(err)
	}
	answered := a.request(node.ID)
	if reply := a.press(editorChat, editor.message, telegramAnswerData(editor.id, 1)); reply != telegramRequestState(answered, "alice") {
		t.Fatalf("press on an answered request = %q", reply)
	}
	a.answeredBy(node.ID, a.alice, "success", "")

	cancelled := a.ask("confirm")
	mine := a.sentRequest(a.alice, cancelled)
	if err := a.srv.store.CancelSwarmRequest(ctx, "s1", cancelled.ID, a.alice, allow); err != nil {
		t.Fatal(err)
	}
	if reply := a.press(ownerChat, mine.message, telegramAnswerData(mine.id, 0)); reply != "This request was cancelled." {
		t.Fatalf("press on a cancelled request = %q", reply)
	}
	a.statusShown(ownerChat, mine, "This request was cancelled.")
	if got := a.request(cancelled.ID); got.State != "cancelled" {
		t.Fatalf("press changed the cancelled request: %+v", got)
	}

	stale := a.ask("confirm")
	mine = a.sentRequest(a.alice, stale)
	a.sql(`UPDATE managed_actors SET phase = 'deleted', desired = 'deleted', deleted_at = 'now' WHERE uid = ?`, a.worker.UID)
	// Отменить такой запрос из brood нечем: ответ называет факт, а не действие.
	if reply := a.press(ownerChat, mine.message, telegramAnswerData(mine.id, 0)); reply != "The asking actor is gone: this request can no longer be answered." {
		t.Fatalf("press on a stale request = %q", reply)
	}
	if got := a.request(stale.ID); got.State != "open" || got.AnsweredBy != "" {
		t.Fatalf("press on a stale request recorded an answer: %+v", got)
	}
}

func TestPanelAnswerEditsTheTelegramMessagesOfReaders(t *testing.T) {
	a := newAnswersHarness(t)
	req := a.openNode()
	mine, editor, viewer := a.sentRequest(a.alice, req), a.sentRequest(a.bob, req), a.sentRequest(a.carol, req)
	// Сообщение, которое Telegram править больше не даёт, — не ошибка:
	// ответ панели проходит, и остальные сообщения правятся.
	var refused atomic.Int32
	a.api.scriptEdit(installBotToken, func(w http.ResponseWriter, e fakeEdit) bool {
		if e.ChatID != editorChat || e.MessageID != editor.message {
			return false
		}
		refused.Add(1)
		fakeRefuse(w, http.StatusBadRequest, "Bad Request: message can't be edited")
		return true
	})
	client := a.signIn("alice")
	resp, data, _ := a.do(client, "POST", "/api/swarms/s1/requests/"+req.ID+"/answer", a.csrf(client), map[string]any{"outcome": "success"})
	expectStatus(t, "panel answer", resp, data, http.StatusOK)
	answered := a.answeredBy(req.ID, a.alice, "success", "")
	a.statusShown(ownerChat, mine, telegramRequestState(answered, "alice"))
	a.statusShown(viewerChat, viewer, telegramRequestState(answered, "alice"))
	telegramEventually(t, "the edit of the editor's message is tried", func() bool { return refused.Load() > 0 })
	time.Sleep(100 * time.Millisecond)
	if refused.Load() != 1 {
		t.Fatalf("the refused edit was tried %d times, want once: no retry", refused.Load())
	}
}

// Ответ, пришедший, пока уведомление запроса было в полёте, правит его
// сразу после отправки: кнопки не остаются у закрытого запроса. Отправка
// правит только своё сообщение, а не все сообщения запроса заново.
func TestRequestAnsweredDuringItsSendGetsItsStatusAfterTheSend(t *testing.T) {
	ctx := context.Background()
	a := newAnswersHarness(t)
	release := make(chan struct{})
	held := a.holdSend(release)
	req := a.openNode()
	waitClosed(t, "a request notification is in flight", held)
	if _, err := a.srv.store.AnswerSwarmRequest(ctx, store.SwarmAnswer{SwarmID: "s1", RequestID: req.ID, UserID: a.alice, Outcome: "success"}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	close(release)
	a.api.scriptSend(installBotToken, nil)
	answered := a.request(req.ID)
	messages := map[int64]requestMessage{}
	for user, chat := range map[string]int64{a.alice: ownerChat, a.bob: editorChat, a.carol: viewerChat} {
		messages[chat] = a.sentRequest(user, req)
		a.statusShown(chat, messages[chat], telegramRequestState(answered, "alice"))
	}
	time.Sleep(100 * time.Millisecond)
	for chat, m := range messages {
		if edits := a.editsOf(chat, m.message); len(edits) != 1 {
			t.Errorf("the message in chat %d was edited %d times, want once: %+v", chat, len(edits), edits)
		}
	}
}

func TestAnswerCommandRefusesWithoutAPendingTextRequest(t *testing.T) {
	a := newAnswersHarness(t)
	node := a.openNode()
	nodeMessage := a.sentRequest(a.alice, node)
	text := a.ask("text")
	mine, editor := a.sentRequest(a.alice, text), a.sentRequest(a.bob, text)
	for _, tc := range []struct{ say, want string }{
		{"/answer", telegramReplyAnswerUsage},
		{"/answer " + strconv.FormatInt(mine.id, 10), telegramReplyAnswerUsage},
		{"/answer 999999 use sqlite", "There is no request 999999 in this chat."},
		{"/answer " + strconv.FormatInt(editor.id, 10) + " use sqlite", "There is no request " + strconv.FormatInt(editor.id, 10) + " in this chat."},
		{"/answer " + strconv.FormatInt(nodeMessage.id, 10) + " ship it", "Request " + strconv.FormatInt(nodeMessage.id, 10) + " is answered with its buttons, not with text."},
	} {
		if got := a.say(ownerChat, tc.say); got != tc.want {
			t.Errorf("%q: reply %q, want %q", tc.say, got, tc.want)
		}
	}
	if got := a.request(text.ID); got.State != "open" {
		t.Fatalf("refused /answer changed the request: %+v", got)
	}
	a.say(ownerChat, "/answer "+strconv.FormatInt(mine.id, 10)+" sqlite")
	answered := a.answeredBy(text.ID, a.alice, "", "sqlite")
	if got := a.say(editorChat, "/answer "+strconv.FormatInt(editor.id, 10)+" postgres"); got != telegramRequestState(answered, "alice") {
		t.Fatalf("/answer on an answered request = %q", got)
	}
	a.answeredBy(text.ID, a.alice, "", "sqlite")
	if got := a.say(viewerChat, "/answer "+strconv.FormatInt(a.sentRequest(a.carol, text).id, 10)+" mysql"); got != telegramReplyDenied {
		t.Fatalf("viewer's /answer = %q", got)
	}
}
