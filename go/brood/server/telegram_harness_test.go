package server

// Поддельный Bot API и сборка сервера для проверок профиля Telegram и
// поллеров (#281): боты по токену, очередь обновлений с подтверждением по
// offset, long poll до timeout запроса, запись getUpdates и sendMessage, а
// для кнопок ответа (#283) и подтверждений команд (#284) — клавиатур,
// нажатий, editMessageReplyMarkup и answerCallbackQuery, для поля ответа
// (#298) — force_reply. Правку разметки сообщения с полем ответа поддельный
// Bot API отвергает, как настоящий (fakeBot.editable).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

// Токены тестовых ботов: форма BotFather, секрет — опознаваемая метка.
const (
	aliceBotToken   = "710000001:SENTINEL-alice_bot-0123456789abcdefghij"
	aliceBotSecret  = "SENTINEL-alice_bot-0123456789abcdefghij"
	bobBotToken     = "710000002:SENTINEL-bob_bot-0123456789abcdefghijkl"
	installBotToken = "710000009:SENTINEL-install_bot-0123456789abcdefgh"
	installSecret   = "SENTINEL-install_bot-0123456789abcdefgh"
)

type fakePoll struct {
	Offset  int64 `json:"offset"`
	Limit   int   `json:"limit"`
	Timeout int   `json:"timeout"`
}

type fakeSent struct {
	ChatID int64
	Text   string
	// Markup — inline-клавиатура сообщения, nil — без неё; ForceReply — поле
	// ответа (force_reply, #298), nil — без него. Обе — из reply_markup.
	Markup     *telegram.InlineKeyboardMarkup
	ForceReply *telegram.ForceReply
	// Document и Name — файл sendDocument; Text у него — подпись.
	Document string
	Name     string
	// ID — номер сообщения, который ответил поддельный Bot API.
	ID int64
}

// fakeMessageBody разбирает JSON sendMessage: чат, текст и reply_markup.
func fakeMessageBody(body []byte) (fakeSent, error) {
	var wire struct {
		ChatID int64           `json:"chat_id"`
		Text   string          `json:"text"`
		Markup json.RawMessage `json:"reply_markup"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return fakeSent{}, err
	}
	s := fakeSent{ChatID: wire.ChatID, Text: wire.Text}
	return s, s.readMarkup(wire.Markup)
}

// readMarkup разбирает reply_markup raw: inline-клавиатуру или поле ответа;
// пусто — без разметки. force_reply не true — не поле ответа: Bot API ждёт
// true.
func (s *fakeSent) readMarkup(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m struct {
		InlineKeyboard [][]telegram.InlineKeyboardButton `json:"inline_keyboard"`
		ForceReply     bool                              `json:"force_reply"`
		Placeholder    string                            `json:"input_field_placeholder"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m.InlineKeyboard != nil {
		s.Markup = &telegram.InlineKeyboardMarkup{InlineKeyboard: m.InlineKeyboard}
	}
	if m.ForceReply {
		s.ForceReply = &telegram.ForceReply{Placeholder: m.Placeholder}
	}
	return nil
}

// fakeAnswer — answerCallbackQuery: id нажатия и текст уведомления.
type fakeAnswer struct {
	ID   string `json:"callback_query_id"`
	Text string `json:"text"`
}

// fakeEdit — editMessageReplyMarkup: сообщение чата и его новая клавиатура,
// nil — клавиатура снята.
type fakeEdit struct {
	ChatID    int64                          `json:"chat_id"`
	MessageID int64                          `json:"message_id"`
	Markup    *telegram.InlineKeyboardMarkup `json:"reply_markup"`
}

type fakeBot struct {
	id       int64
	username string
	updates  []map[string]any
	nextID   int64
	polls    []fakePoll
	sent     []fakeSent
	// answers и edits — принятые ответы на нажатия и правки клавиатур;
	// refused — правки, которые Bot API отверг по своему правилу (editable).
	answers  []fakeAnswer
	edits    []fakeEdit
	refused  []fakeEdit
	inflight int
	// meCalls — сколько раз спросили getMe.
	meCalls int
	// answer — сценарий ответа getUpdates: true — ответ записан, очередь
	// не тронута.
	answer func(w http.ResponseWriter, p fakePoll) bool
	// meDown — getMe отвечает 502, как недоступный Bot API.
	meDown bool
	// me и send — сценарии getMe и sendMessage: true — ответ записан, и
	// обычного ответа нет.
	me   func(w http.ResponseWriter) bool
	send func(w http.ResponseWriter, r *http.Request, s fakeSent) bool
	// edit — сценарий editMessageReplyMarkup: true — ответ записан, правки
	// нет.
	edit func(w http.ResponseWriter, e fakeEdit) bool
	// onAnswer вызывается на answerCallbackQuery до ответа: поллер ждёт
	// его после своей проверки привязки и до действия нажатия.
	onAnswer func()
}

type fakeBotAPI struct {
	t    *testing.T
	url  string
	mu   sync.Mutex
	bots map[string]*fakeBot
	// wake закрывается и заменяется при каждом изменении очередей.
	wake chan struct{}
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	api := &fakeBotAPI{t: t, bots: map[string]*fakeBot{}, wake: make(chan struct{})}
	s := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(s.Close)
	api.url = s.URL
	return api
}

// addBot заводит бота: id — часть токена до двоеточия.
func (a *fakeBotAPI) addBot(token, username string) {
	var id int64
	if _, err := fmt.Sscanf(token, "%d:", &id); err != nil {
		a.t.Fatalf("bot token %q has no id", token)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bots[token] = &fakeBot{id: id, username: username, nextID: 1000}
}

// editable: Bot API правит разметку сообщения правки e. Правит он только
// сообщения без разметки или с inline-клавиатурой (раздел «Updating
// messages»), и правку сообщения, ушедшего с полем ответа (force_reply),
// отвергает. Сообщение, которого бот не отправлял, правило не трогает: его
// правку решает сценарий. Вызывается под a.mu.
func (b *fakeBot) editable(e fakeEdit) bool {
	if e.MessageID < 1 || int(e.MessageID) > len(b.sent) {
		return true
	}
	s := b.sent[e.MessageID-1]
	return s.ChatID != e.ChatID || s.ForceReply == nil
}

func (a *fakeBotAPI) bot(token string) *fakeBot {
	b := a.bots[token]
	if b == nil {
		a.t.Fatalf("no fake bot for the token")
	}
	return b
}

// notify будит ждущие long poll.
func (a *fakeBotAPI) notify() {
	close(a.wake)
	a.wake = make(chan struct{})
}

// push ставит обновление в очередь бота и возвращает его update_id.
func (a *fakeBotAPI) push(token string, update map[string]any) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.bot(token)
	id := b.nextID
	b.nextID++
	update["update_id"] = id
	b.updates = append(b.updates, update)
	a.notify()
	return id
}

// script задаёт сценарий ответа getUpdates; nil — обычная очередь.
func (a *fakeBotAPI) script(token string, answer func(w http.ResponseWriter, p fakePoll) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bot(token).answer = answer
	a.notify()
}

func (a *fakeBotAPI) polls(token string) []fakePoll {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]fakePoll(nil), a.bot(token).polls...)
}

func (a *fakeBotAPI) sent(token string) []fakeSent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]fakeSent(nil), a.bot(token).sent...)
}

func (a *fakeBotAPI) sentTo(token string, chat int64) []string {
	var texts []string
	for _, s := range a.sent(token) {
		if s.ChatID == chat {
			texts = append(texts, s.Text)
		}
	}
	return texts
}

func (a *fakeBotAPI) answers(token string) []fakeAnswer {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]fakeAnswer(nil), a.bot(token).answers...)
}

func (a *fakeBotAPI) edits(token string) []fakeEdit {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]fakeEdit(nil), a.bot(token).edits...)
}

// refusedEdits — правки бота token, которые Bot API отверг: сообщение ушло с
// полем ответа (editable).
func (a *fakeBotAPI) refusedEdits(token string) []fakeEdit {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]fakeEdit(nil), a.bot(token).refused...)
}

func (a *fakeBotAPI) inflight(token string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bot(token).inflight
}

func (a *fakeBotAPI) meCalls(token string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bot(token).meCalls
}

// confirmed: поллер подтвердил обновление id — пришёл getUpdates с offset
// дальше него. Обработка обновления к этому моменту закончена: поллер
// просит следующую порцию только после неё.
func (a *fakeBotAPI) confirmed(token string, id int64) bool {
	for _, p := range a.polls(token) {
		if p.Offset > id {
			return true
		}
	}
	return false
}

func (a *fakeBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/bot")
	token, method, found := strings.Cut(rest, "/")
	if !ok || !found || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	b := a.bots[token]
	a.mu.Unlock()
	if b == nil {
		fakeRefuse(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	switch method {
	case "getMe":
		a.mu.Lock()
		b.meCalls++
		down, me := b.meDown, b.me
		a.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if me != nil && me(w) {
			return
		}
		fakeResult(w, map[string]any{"id": b.id, "is_bot": true, "first_name": "Bot", "username": b.username})
	case "getUpdates":
		var p fakePoll
		if err := json.Unmarshal(body, &p); err != nil {
			a.t.Errorf("getUpdates body %q: %v", body, err)
		}
		a.getUpdates(w, r, b, p)
	case "sendMessage":
		s, err := fakeMessageBody(body)
		if err != nil {
			a.t.Errorf("sendMessage body %q: %v", body, err)
		}
		a.mu.Lock()
		send := b.send
		a.mu.Unlock()
		if send != nil && send(w, r, s) {
			return
		}
		a.mu.Lock()
		s.ID = int64(len(b.sent) + 1)
		b.sent = append(b.sent, s)
		a.notify()
		a.mu.Unlock()
		fakeResult(w, map[string]any{"message_id": s.ID, "chat": map[string]any{"id": s.ChatID, "type": "private"}, "date": 1763000000, "text": s.Text})
	case "answerCallbackQuery":
		var answer fakeAnswer
		if err := json.Unmarshal(body, &answer); err != nil {
			a.t.Errorf("answerCallbackQuery body %q: %v", body, err)
		}
		a.mu.Lock()
		b.answers = append(b.answers, answer)
		onAnswer := b.onAnswer
		a.notify()
		a.mu.Unlock()
		if onAnswer != nil {
			onAnswer()
		}
		fakeResult(w, true)
	case "editMessageReplyMarkup":
		var e fakeEdit
		if err := json.Unmarshal(body, &e); err != nil {
			a.t.Errorf("editMessageReplyMarkup body %q: %v", body, err)
		}
		a.mu.Lock()
		edit, editable := b.edit, b.editable(e)
		if !editable {
			b.refused = append(b.refused, e)
			a.notify()
		}
		a.mu.Unlock()
		if !editable {
			fakeRefuse(w, http.StatusBadRequest, "Bad Request: message can't be edited")
			return
		}
		if edit != nil && edit(w, e) {
			return
		}
		a.mu.Lock()
		b.edits = append(b.edits, e)
		a.notify()
		a.mu.Unlock()
		fakeResult(w, true)
	case "sendDocument":
		s, ok := fakeDocumentForm(r, body)
		if !ok {
			a.t.Errorf("sendDocument form %q", body)
			fakeRefuse(w, http.StatusBadRequest, "Bad Request: bad form")
			return
		}
		a.mu.Lock()
		send := b.send
		a.mu.Unlock()
		if send != nil && send(w, r, s) {
			return
		}
		a.mu.Lock()
		s.ID = int64(len(b.sent) + 1)
		b.sent = append(b.sent, s)
		a.notify()
		a.mu.Unlock()
		fakeResult(w, map[string]any{"message_id": s.ID, "chat": map[string]any{"id": s.ChatID, "type": "private"}, "date": 1763000000, "caption": s.Text})
	default:
		fakeRefuse(w, http.StatusNotFound, "Not Found")
	}
}

// fakeDocumentForm разбирает multipart sendDocument: чат, подпись и файл.
func fakeDocumentForm(r *http.Request, body []byte) (fakeSent, bool) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return fakeSent{}, false
	}
	form, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(8 << 20)
	if err != nil {
		return fakeSent{}, false
	}
	var s fakeSent
	if _, err := fmt.Sscan(strings.Join(form.Value["chat_id"], ""), &s.ChatID); err != nil {
		return fakeSent{}, false
	}
	s.Text = strings.Join(form.Value["caption"], "")
	if err := s.readMarkup([]byte(strings.Join(form.Value["reply_markup"], ""))); err != nil {
		return fakeSent{}, false
	}
	files := form.File["document"]
	if len(files) != 1 {
		return fakeSent{}, false
	}
	f, err := files[0].Open()
	if err != nil {
		return fakeSent{}, false
	}
	defer f.Close()
	content, err := io.ReadAll(f)
	if err != nil {
		return fakeSent{}, false
	}
	s.Document, s.Name = string(content), files[0].Filename
	return s, true
}

func (a *fakeBotAPI) getUpdates(w http.ResponseWriter, r *http.Request, b *fakeBot, p fakePoll) {
	a.mu.Lock()
	b.polls = append(b.polls, p)
	b.inflight++
	a.notify()
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		b.inflight--
		a.notify()
		a.mu.Unlock()
	}()
	deadline := time.After(time.Duration(p.Timeout) * time.Second)
	for {
		a.mu.Lock()
		if answer := b.answer; answer != nil {
			a.mu.Unlock()
			if answer(w, p) {
				return
			}
			a.mu.Lock()
		}
		// Обновления до offset подтверждены: Telegram их забывает.
		kept := b.updates[:0]
		for _, u := range b.updates {
			if u["update_id"].(int64) >= p.Offset {
				kept = append(kept, u)
			}
		}
		b.updates = kept
		batch := b.updates
		if p.Limit > 0 && len(batch) > p.Limit {
			batch = batch[:p.Limit]
		}
		batch = append([]map[string]any(nil), batch...)
		wake := a.wake
		a.mu.Unlock()
		if len(batch) > 0 || p.Timeout == 0 {
			fakeResult(w, batch)
			return
		}
		select {
		case <-wake:
		case <-deadline:
			fakeResult(w, []map[string]any{})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func fakeResult(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v})
}

func fakeRefuse(w http.ResponseWriter, code int, description string) {
	fakeRefuseRetry(w, code, description, 0)
}

// fakeRefuseRetry — отказ Bot API; retryAfter в секундах, 0 — без него.
func fakeRefuseRetry(w http.ResponseWriter, code int, description string, retryAfter int) {
	answer := map[string]any{"ok": false, "error_code": code, "description": description}
	if retryAfter > 0 {
		answer["parameters"] = map[string]any{"retry_after": retryAfter}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(answer)
}

// privateMessage — сообщение личного чата: id чата равен id собеседника.
func privateMessage(from int64, username, text string) map[string]any {
	return chatMessage(from, "private", from, username, text)
}

// callbackPress — нажатие кнопки data под сообщением message личного чата
// from; id — id нажатия, повтор того же нажатия Telegram шлёт с тем же id.
func callbackPress(id string, from, message int64, data string) map[string]any {
	return callbackPressIn(id, from, "private", from, false, message, data)
}

// callbackPressIn — нажатие кнопки data под сообщением message в чате chat
// вида kind от собеседника from; bot — собеседник бот.
func callbackPressIn(id string, chat int64, kind string, from int64, bot bool, message int64, data string) map[string]any {
	return map[string]any{"callback_query": map[string]any{"id": id, "data": data,
		"from":    map[string]any{"id": from, "is_bot": bot, "first_name": "Person"},
		"message": map[string]any{"message_id": message, "chat": map[string]any{"id": chat, "type": kind}, "date": 1763000000}}}
}

func chatMessage(chat int64, kind string, from int64, username, text string) map[string]any {
	sender := map[string]any{"id": from, "is_bot": false, "first_name": "Person"}
	if username != "" {
		sender["username"] = username
	}
	return map[string]any{"message": map[string]any{"message_id": 1, "from": sender,
		"chat": map[string]any{"id": chat, "type": kind}, "date": 1763000000, "text": text}}
}

// telegramClock — часы поллеров и выдачи кода, которые тест двигает сам.
type telegramClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *telegramClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *telegramClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// telegramHarness — сервер с поддельным Bot API, сжатым ритмом поллеров и
// своими часами.
type telegramHarness struct {
	*testServer
	api   *fakeBotAPI
	clock *telegramClock
}

// newTelegramHarness собирает сервер; install — токен бота установки, пусто
// — без него. prepare пишет в базу до старта сервера.
func newTelegramHarness(t *testing.T, install string, prepare func(ctx context.Context, st *store.Store)) *telegramHarness {
	t.Helper()
	return startTelegramHarness(t, newTestBotAPI(t), "", install, prepare)
}

// newTestBotAPI — поддельный Bot API с ботами alice, bob и установки.
func newTestBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	api := newFakeBotAPI(t)
	api.addBot(aliceBotToken, "Alice_Bot")
	api.addBot(bobBotToken, "bob_bot")
	api.addBot(installBotToken, "brood_bot")
	return api
}

// getMeDown переключает отказ getMe бота token.
func (a *fakeBotAPI) getMeDown(token string, down bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bot(token).meDown = down
}

// scriptMe задаёт сценарий getMe бота token; nil — обычный ответ.
func (a *fakeBotAPI) scriptMe(token string, me func(w http.ResponseWriter) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bot(token).me = me
}

// scriptEdit задаёт сценарий editMessageReplyMarkup бота token; nil —
// обычный ответ.
func (a *fakeBotAPI) scriptEdit(token string, edit func(w http.ResponseWriter, e fakeEdit) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bot(token).edit = edit
}

// scriptSend задаёт сценарий sendMessage бота token; nil — обычный ответ.
func (a *fakeBotAPI) scriptSend(token string, send func(w http.ResponseWriter, r *http.Request, s fakeSent) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bot(token).send = send
}

// scriptAnswer задаёт действие на answerCallbackQuery бота token; nil — без
// него.
func (a *fakeBotAPI) scriptAnswer(token string, onAnswer func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bot(token).onAnswer = onAnswer
}

// startTelegramHarness собирает сервер над api; apiURL — origin Bot API
// сервера, пусто — api. tune правит настройки поллеров сборки.
func startTelegramHarness(t *testing.T, api *fakeBotAPI, apiURL, install string, prepare func(ctx context.Context, st *store.Store), tune ...func(*telegramOptions)) *telegramHarness {
	t.Helper()
	return startTelegramHarnessAt(t, t.TempDir()+"/brood.db", api, apiURL, install, prepare, tune...)
}

// startTelegramHarnessAt — startTelegramHarness над базой dbPath: проверка
// перезапуска готовит базу сама, как её оставил прежний процесс.
func startTelegramHarnessAt(t *testing.T, dbPath string, api *fakeBotAPI, apiURL, install string, prepare func(ctx context.Context, st *store.Store), tune ...func(*telegramOptions)) *telegramHarness {
	t.Helper()
	if apiURL == "" {
		apiURL = api.url
	}
	ctx := context.Background()
	// Ссылки уведомлений ведут на панель этого origin.
	cfg := Config{DBPath: dbPath, TelegramAPI: apiURL, telegramBotToken: install, PanelOrigin: "https://brood.example"}
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(ctx, st)
	}
	clock := &telegramClock{now: time.Now().UTC().Truncate(time.Second)}
	options := telegramOptions{
		wait:      time.Second,
		reconcile: 50 * time.Millisecond,
		backoff:   func(int) time.Duration { return 5 * time.Millisecond },
		now:       clock.Now,
		scan:      20 * time.Millisecond,
	}
	for _, f := range tune {
		f(&options)
	}
	opts := Options{Provisioner: newFakeProvisioner(), telegram: options, argon2: cheapArgon2}
	srv, err := NewFromStore(cfg, opts, st)
	if err != nil {
		st.Close()
		t.Fatalf("NewFromStore: %v", err)
	}
	ts := &testServer{t: t, srv: srv, provisioner: opts.Provisioner, dbPath: cfg.DBPath}
	ts.http = httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.http.Close()
		srv.Close()
	})
	return &telegramHarness{testServer: ts, api: api, clock: clock}
}

// createTelegramUser заводит пользователя в базе и возвращает его id.
func createTelegramUser(t *testing.T, ctx context.Context, st *store.Store, username string) string {
	t.Helper()
	hash, err := auth.HashPassword(username+"-password", cheapArgon2)
	if err != nil {
		t.Fatal(err)
	}
	id := store.NewID()
	if err := st.CreateUser(ctx, store.User{ID: id, Username: username, PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	return id
}

// member заводит пользователя и входит им; возвращает клиент и id.
func (h *telegramHarness) member(username string) (*http.Client, string) {
	h.t.Helper()
	id := createTelegramUser(h.t, context.Background(), h.srv.store, username)
	return h.signIn(username), id
}

// signIn входит уже заведённым пользователем.
func (h *telegramHarness) signIn(username string) *http.Client {
	h.t.Helper()
	client := newClient(h.t)
	h.login(client, username, username+"-password")
	return client
}

func (h *telegramHarness) link(userID string) store.TelegramLink {
	h.t.Helper()
	link, err := h.srv.store.GetTelegramLink(context.Background(), userID)
	if err != nil {
		h.t.Fatal(err)
	}
	return link
}

// setBot записывает свой бот пользователя через API и ждёт его поллер.
func (h *telegramHarness) setBot(client *http.Client, userID, token string) {
	h.t.Helper()
	resp, data, _ := h.do(client, "PUT", "/api/me/telegram", h.csrf(client), map[string]any{"token": token})
	expectStatus(h.t, "PUT /api/me/telegram", resp, data, http.StatusOK)
	telegramEventually(h.t, "the poller of the new bot polls", func() bool { return len(h.api.polls(token)) > 0 })
}

// issueCode выдаёт код привязки через API.
func (h *telegramHarness) issueCode(client *http.Client) map[string]any {
	h.t.Helper()
	data, _ := h.issueCodeRaw(client)
	return data
}

// issueCodeRaw выдаёт код привязки через API и возвращает и сырое тело
// ответа: проверка write-only смотрит каждое его поле.
func (h *telegramHarness) issueCodeRaw(client *http.Client) (map[string]any, string) {
	h.t.Helper()
	resp, data, raw := h.do(client, "POST", "/api/me/telegram/link", h.csrf(client), nil)
	expectStatus(h.t, "POST /api/me/telegram/link", resp, data, http.StatusCreated)
	return data, raw
}

// deliver ставит обновление и ждёт, пока поллер его обработает.
func (h *telegramHarness) deliver(token string, update map[string]any) {
	h.t.Helper()
	id := h.api.push(token, update)
	telegramEventually(h.t, "the poller confirms the update", func() bool { return h.api.confirmed(token, id) })
}

// linkChat привязывает личный чат chat к пользователю через бота token.
func (h *telegramHarness) linkChat(client *http.Client, token string, chat int64, username string) {
	h.t.Helper()
	code := h.issueCode(client)["code"].(string)
	h.deliver(token, privateMessage(chat, username, "/start "+code))
}

func telegramEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// quiet: за время d поллер бота token не спрашивает getUpdates и ничего
// не держит в полёте.
func (h *telegramHarness) quiet(token string, d time.Duration) bool {
	before := len(h.api.polls(token))
	time.Sleep(d)
	return h.api.inflight(token) == 0 && len(h.api.polls(token)) == before
}

// sql выполняет statement в базе сервера своим соединением, мимо
// хранилища: так тест ставит и снимает триггер сбоя записи.
func (h *telegramHarness) sql(statement string, args ...any) {
	h.t.Helper()
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA busy_timeout=10000`); err != nil {
		h.t.Fatal(err)
	}
	if _, err := db.Exec(statement, args...); err != nil {
		h.t.Fatalf("%s: %v", statement, err)
	}
}

// telegramSessions — число сессий вида telegram пользователя.
func (h *telegramHarness) telegramSessions(userID string) int {
	h.t.Helper()
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id = ? AND kind = 'telegram'`, userID).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// lockWrites берёт блокировку записи базы своим соединением, мимо
// хранилища: запись хранилища ждёт её в busy_timeout, пока тест не позовёт
// release.
func (h *telegramHarness) lockWrites() (release func()) {
	h.t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		h.t.Fatal(err)
	}
	for _, statement := range []string{`PRAGMA busy_timeout=10000`, `BEGIN IMMEDIATE`} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			conn.Close()
			db.Close()
			h.t.Fatalf("%s: %v", statement, err)
		}
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
				h.t.Error(err)
			}
			conn.Close()
			db.Close()
		})
	}
	h.t.Cleanup(release)
	return release
}

// column читает одно значение из базы сервера своим соединением, мимо
// хранилища: так тест видит базу и после Server.Close.
func (h *telegramHarness) column(query string, args ...any) string {
	h.t.Helper()
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	var value sql.NullString
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
	return value.String
}

// halted: поллер своего бота пользователя остановлен 401 или 409, и сверка
// его не запускает.
func (h *telegramHarness) halted(userID string) bool {
	b := h.srv.telegram
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.halted[userID]
	return ok
}

// breakReconcile портит строку привязки постороннего пользователя так, что
// хранилище не может её прочесть: сверка набора поллеров отказывает, а
// записи вызывающего идут.
func (h *telegramHarness) breakReconcile() {
	h.t.Helper()
	ctx := context.Background()
	carolID := createTelegramUser(h.t, ctx, h.srv.store, "carol")
	if _, err := h.srv.store.SetTelegramEvents(ctx, carolID, []string{"request_opened"}); err != nil {
		h.t.Fatal(err)
	}
	h.sql(`UPDATE user_telegram_links SET events = '[1]' WHERE user_id = ?`, carolID)
	if _, _, err := h.srv.telegram.desired(ctx); err == nil {
		h.t.Fatal("the pollers still reconcile with an unreadable link")
	}
}

// confirmInStore подтверждает код привязки хранилищем, мимо поллера: без
// просьбы о сверке, которую шлёт поллер после привязки.
func (h *telegramHarness) confirmInStore(botUsername, code string, chat int64) {
	h.t.Helper()
	_, err := h.srv.store.ConfirmTelegramLink(context.Background(), store.TelegramLinkConfirmation{
		BotUsername: botUsername, Code: code, ChatID: chat, TelegramUserID: chat, Now: h.clock.Now(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

// poller — поллер пользователя userID ("" — бота установки), nil — его нет.
func (h *telegramHarness) poller(userID string) *telegramPoller {
	b := h.srv.telegram
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	return b.pollers[userID]
}

// answersAfterSync шлёт запрос записи профиля с телом body (nil — без
// тела), пока сверка держит набор поллеров, и проверяет, что ответ ждёт её:
// обработчик сверяет набор сам, а не просит сверку на потом. Ответ приходит
// после того, как набор отпущен.
func (h *telegramHarness) answersAfterSync(client *http.Client, method, path string, body any) (*http.Response, map[string]any) {
	h.t.Helper()
	csrf := h.csrf(client)
	type answer struct {
		resp *http.Response
		data map[string]any
		err  error
	}
	answered := make(chan answer, 1)
	b := h.srv.telegram
	b.syncMu.Lock()
	go func() {
		resp, data, err := telegramRequest(context.Background(), h.http.URL, client, method, path, csrf, body)
		answered <- answer{resp, data, err}
	}()
	select {
	case got := <-answered:
		b.syncMu.Unlock()
		h.t.Fatalf("%s %s answered before the pollers were reconciled: %v %v", method, path, got.data, got.err)
	case <-time.After(300 * time.Millisecond):
	}
	b.syncMu.Unlock()
	got := <-answered
	if got.err != nil {
		h.t.Fatalf("%s %s: %v", method, path, got.err)
	}
	return got.resp, got.data
}

// telegramRequest — запрос записи с JSON-телом body (nil — без тела) в
// контексте ctx, годный для чужой горутины: не зовёт t.Fatal.
func telegramRequest(ctx context.Context, base string, client *http.Client, method, path, csrf string, body any) (*http.Response, map[string]any, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", base)
	req.Header.Set(auth.HeaderCSRF, csrf)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	return resp, data, nil
}
