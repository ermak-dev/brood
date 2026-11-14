package server

// FEATURE: Answer buttons of Telegram notifications (#283, epic #278).
// SPEC: a request notification of a recipient who may manage the swarm
// carries answer controls while the request is open: a choice gets one
// button per option, a confirm gets Yes and No, a text request names a reply
// and the /answer command with the notification number, in the caption too
// when the text goes as a document, and opens the reply field (force_reply,
// #298) on the message or the document, so a plain reply in the chat answers
// the request; the placeholder of the field names the request by the same
// number as /answer. A message carries one markup: buttons or the reply
// field, never both. A choice whose options Telegram would
// refuse as buttons (a blank label, more options than buttons) gets none and
// says to answer in brood. A viewer, a closed request and every other kind
// get none. callback_data carries only the notification
// number and the option index, never the option text, within the Bot API
// bound of 64 bytes, and anything else is refused; a pressed option maps to
// the answer the store takes: the route outcome of a node request, the label
// of an actor choice, yes or no of a confirm. A closed request shows who
// answered and when, with the chosen option, on one status button. /answer
// takes the number and the text, keeping the lines of the text.
// IMPLEMENTATION: telegram_buttons.go, renderTelegramNotice
// (telegram_notice.go).
// STATUS: DONE

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

func actorRequestOf(kind string) store.SwarmRequest {
	r := store.SwarmRequest{ID: "r2", SwarmID: "s", Origin: "actor", Kind: kind, Question: "Which db?", State: "open"}
	if kind == "choice" {
		r.Options = []store.RequestOption{{Label: "sqlite", Outcome: "sqlite"}, {Label: "postgres", Outcome: "postgres"}}
	}
	return r
}

func TestButtonDataCarriesTheNotificationAndTheOptionOnly(t *testing.T) {
	for _, tc := range []struct {
		data   string
		button telegramButton
	}{
		{telegramAnswerData(7, 0), telegramButton{notification: 7, option: 0}},
		{telegramAnswerData(math.MaxInt64, 15), telegramButton{notification: math.MaxInt64, option: 15}},
		{telegramStatusData(42), telegramButton{notification: 42, status: true}},
	} {
		if len(tc.data) > 64 {
			t.Errorf("callback data %q is %d bytes, over the Bot API bound", tc.data, len(tc.data))
		}
		if got, ok := parseTelegramButton(tc.data); !ok || got != tc.button {
			t.Errorf("parseTelegramButton(%q) = %+v, %t; want %+v", tc.data, got, ok, tc.button)
		}
	}
	for _, data := range []string{"", "a:", "a:7", "a:7:", "a:0:1", "a:-7:1", "a:+7:1", "a:07:1", "a:7:01", "a:7:-1", "a:7:100", "a:7:1:2",
		"s:", "s:0", "s:7:1", "x:7:1", "a:99999999999999999999:1", "Ship", "a:7:Ship"} {
		if got, ok := parseTelegramButton(data); ok {
			t.Errorf("parseTelegramButton(%q) = %+v, want a refusal", data, got)
		}
	}
}

func TestChoiceKeyboardHasAButtonPerOptionWithoutTheOptionInItsData(t *testing.T) {
	node := nodeRequestFacts().request
	node.State = "open"
	keyboard := telegramRequestKeyboard(9, *node)
	want := &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: "Ship", CallbackData: "a:9:0"}},
		{{Text: "Hold", CallbackData: "a:9:1"}},
	}}
	if !reflect.DeepEqual(keyboard, want) {
		t.Fatalf("node keyboard = %+v, want %+v", keyboard, want)
	}
	if got := telegramRequestKeyboard(9, actorRequestOf("confirm")); !reflect.DeepEqual(got, &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: "Yes", CallbackData: "a:9:0"}, {Text: "No", CallbackData: "a:9:1"}},
	}}) {
		t.Fatalf("confirm keyboard = %+v", got)
	}
	if got := telegramRequestKeyboard(9, actorRequestOf("text")); got != nil {
		t.Fatalf("text request keyboard = %+v, want none", got)
	}
}

// Текстовый запрос открывает поле ответа: клиент Telegram сам начинает reply
// на сообщение запроса. Подсказка пустого поля называет запрос тем же
// номером, что /answer в тексте сообщения; запрос ответа выбирает сообщение,
// на которое сделан reply, а не подсказка. Запрос с кнопками поля не
// открывает.
func TestOnlyATextRequestOpensTheReplyField(t *testing.T) {
	if got := telegramAnswerField(7, actorRequestOf("text")); !reflect.DeepEqual(got, &telegram.ForceReply{Placeholder: "Answer to request 7"}) {
		t.Fatalf("text request reply field = %+v", got)
	}
	// Bot API держит подсказку в 64 знаках, и самый длинный номер в них
	// помещается.
	if got := telegramAnswerField(math.MaxInt64, actorRequestOf("text")); got == nil || telegram.TextLength(got.Placeholder) > 64 {
		t.Fatalf("reply field of the largest notification number = %+v", got)
	}
	node := *nodeRequestFacts().request
	for name, r := range map[string]store.SwarmRequest{"node": node, "choice": actorRequestOf("choice"), "confirm": actorRequestOf("confirm")} {
		if got := telegramAnswerField(7, r); got != nil {
			t.Errorf("%s request reply field = %+v, want none", name, got)
		}
	}
}

func TestPressedOptionIsTheAnswerTheStoreTakes(t *testing.T) {
	node := *nodeRequestFacts().request
	for _, tc := range []struct {
		name              string
		request           store.SwarmRequest
		option            int
		outcome, response string
		ok                bool
	}{
		{"node option", node, 1, "fail", "", true},
		{"actor choice", actorRequestOf("choice"), 1, "", "postgres", true},
		{"confirm yes", actorRequestOf("confirm"), 0, "", "yes", true},
		{"confirm no", actorRequestOf("confirm"), 1, "", "no", true},
		{"node option out of range", node, 2, "", "", false},
		{"confirm out of range", actorRequestOf("confirm"), 2, "", "", false},
		{"text", actorRequestOf("text"), 0, "", "", false},
	} {
		outcome, response, ok := telegramButtonAnswer(tc.request, tc.option)
		if outcome != tc.outcome || response != tc.response || ok != tc.ok {
			t.Errorf("%s: answer %q, %q, %t; want %q, %q, %t", tc.name, outcome, response, ok, tc.outcome, tc.response, tc.ok)
		}
	}
}

func TestRequestStateNamesWhoAnsweredAndWhen(t *testing.T) {
	at := "2026-11-14T12:03:09.123456Z"
	outcome, yes, text := "fail", "yes", "use sqlite"
	node := *nodeRequestFacts().request
	node.State, node.AnsweredAt, node.Outcome = "answered", &at, &outcome
	confirm := actorRequestOf("confirm")
	confirm.State, confirm.AnsweredAt, confirm.Response = "answered", &at, &yes
	written := actorRequestOf("text")
	written.State, written.AnsweredAt, written.Response = "answered", &at, &text
	cancelled := actorRequestOf("text")
	cancelled.State = "cancelled"
	for _, tc := range []struct {
		request store.SwarmRequest
		want    string
	}{
		{node, "Answered by Alice at 2026-11-14 12:03 UTC: Hold."},
		{confirm, "Answered by Alice at 2026-11-14 12:03 UTC: yes."},
		{written, "Answered by Alice at 2026-11-14 12:03 UTC."},
		{cancelled, "This request was cancelled."},
		{actorRequestOf("text"), "This request is open."},
	} {
		if got := telegramRequestState(tc.request, "Alice"); got != tc.want {
			t.Errorf("state of a %s %s request = %q, want %q", tc.request.State, tc.request.Kind, got, tc.want)
		}
	}
	status := telegramStatusKeyboard(9, telegramRequestState(node, "Alice"))
	if !reflect.DeepEqual(status, &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: "Answered by Alice at 2026-11-14 12:03 UTC: Hold.", CallbackData: "s:9"}},
	}}) {
		t.Fatalf("status keyboard = %+v", status)
	}
	long := telegramStatusKeyboard(9, strings.Repeat("Answered ", 100))
	if n := telegram.TextLength(long.InlineKeyboard[0][0].Text); n > telegramStatusLimit {
		t.Fatalf("status button of %d characters", n)
	}
}

func TestAnswerCommandTakesTheNumberAndTheText(t *testing.T) {
	for _, tc := range []struct {
		text   string
		id     int64
		answer string
		ok     bool
	}{
		{"/answer 12 use sqlite", 12, "use sqlite", true},
		{"  /answer@Brood_Bot   12   use sqlite\nand  WAL  ", 12, "use sqlite\nand  WAL", true},
		{"/answer\n12\nfirst line\nsecond line", 12, "first line\nsecond line", true},
		// Команда в любом регистре, как команды чата (#284).
		{"/ANSWER@Brood_Bot 12 use sqlite", 12, "use sqlite", true},
		{"/Answer 12 use sqlite", 12, "use sqlite", true},
		{"/answer 12", 0, "", false},
		{"/answer 12   ", 0, "", false},
		{"/answer", 0, "", false},
		{"/answer x use sqlite", 0, "", false},
		{"/answer 0 use sqlite", 0, "", false},
		{"/answer -3 use sqlite", 0, "", false},
		{"/answer 012 use sqlite", 0, "", false},
		{"/answers 12 use sqlite", 0, "", false},
		{"answer 12 use sqlite", 0, "", false},
	} {
		id, answer, ok := telegramAnswerArgs(tc.text)
		if id != tc.id || answer != tc.answer || ok != tc.ok {
			t.Errorf("telegramAnswerArgs(%q) = %d, %q, %t; want %d, %q, %t", tc.text, id, answer, ok, tc.id, tc.answer, tc.ok)
		}
	}
}

// Кнопки получает только тот, кто вправе управлять роем, и только у
// открытого запроса; права всё равно перепроверяет транзакция ответа.
func TestNoticeOffersAnswerControlsOnlyToAManagerOfAnOpenRequest(t *testing.T) {
	ev := store.TelegramEvent{Swarm: "s/1", Request: "r1", Sequence: 3, Node: "approve"}
	open := nodeRequestFacts()
	open.request.State = "open"
	manager := store.TelegramNotification{ID: 7, Kind: "request_opened", Event: ev, Manager: true}
	notice := renderTelegramNotice(manager, open, "")
	if !reflect.DeepEqual(notice.markup, telegramRequestKeyboard(7, *open.request)) {
		t.Fatalf("manager's node request markup = %+v", notice.markup)
	}
	contains(t, "manager's node request", notice.text, "a button below")
	viewer := manager
	viewer.Manager = false
	if notice := renderTelegramNotice(viewer, open, ""); notice.markup != nil || strings.Contains(notice.text, "button") {
		t.Fatalf("viewer's node request offers controls: %+v", notice)
	}
	closed := nodeRequestFacts()
	closed.request.State = "answered"
	if notice := renderTelegramNotice(manager, closed, ""); notice.markup != nil {
		t.Fatalf("answered request offers controls: %+v", notice.markup)
	}
	answered := manager
	answered.Kind = "request_answered"
	if notice := renderTelegramNotice(answered, open, ""); notice.markup != nil {
		t.Fatalf("request_answered offers controls: %+v", notice.markup)
	}
	// Закрытый к отправке текстовый запрос поля ответа не открывает.
	closedText := actorRequestOf("text")
	closedText.State = "answered"
	closedQuestion := store.TelegramNotification{ID: 7, Kind: "actor_question", Event: store.TelegramEvent{Swarm: "s", Request: "r2"}, Manager: true}
	if notice := renderTelegramNotice(closedQuestion, telegramFacts{swarm: "build", actor: "builder-1", request: &closedText}, ""); notice.markup != nil {
		t.Fatalf("answered text question opens the reply field: %+v", notice.markup)
	}
	// Разметка у сообщения одна: кнопки выбора и confirm или поле ответа
	// текстового запроса.
	for kind, want := range map[string]struct {
		hint   string
		markup telegram.ReplyMarkup
	}{
		"choice":  {"answer with a button below", telegramRequestKeyboard(7, actorRequestOf("choice"))},
		"confirm": {"answer with a button below", telegramRequestKeyboard(7, actorRequestOf("confirm"))},
		"text":    {"send /answer 7 <your answer>", &telegram.ForceReply{Placeholder: "Answer to request 7"}},
	} {
		f := telegramFacts{swarm: "build", actor: "builder-1"}
		request := actorRequestOf(kind)
		f.request = &request
		question := store.TelegramNotification{ID: 7, Kind: "actor_question", Event: store.TelegramEvent{Swarm: "s", Request: "r2"}, Manager: true}
		notice := renderTelegramNotice(question, f, "")
		contains(t, "manager's "+kind+" question", notice.text, want.hint)
		if !reflect.DeepEqual(notice.markup, want.markup) {
			t.Errorf("manager's %s question markup = %+v, want %+v", kind, notice.markup, want.markup)
		}
		question.Manager = false
		if notice := renderTelegramNotice(question, f, ""); notice.markup != nil || strings.Contains(notice.text, "/answer") {
			t.Errorf("viewer's %s question offers controls: %+v", kind, notice)
		}
	}
}

// Кнопки идут и с документом: длинный материал не отнимает ответа, и
// подпись документа говорит, как ответить.
func TestLongRequestKeepsItsButtonsOnTheDocument(t *testing.T) {
	f := nodeRequestFacts()
	f.request.State = "open"
	f.request.Context = strings.Repeat("Line of the step report.\n", 700)
	notice := renderTelegramNotice(store.TelegramNotification{ID: 7, Kind: "request_opened", Event: store.TelegramEvent{Swarm: "s/1", Request: "r1", Sequence: 3, Node: "approve"}, Manager: true}, f, "")
	if notice.doc == nil || !reflect.DeepEqual(notice.markup, telegramRequestKeyboard(7, *f.request)) {
		t.Fatalf("long request: document %t, markup %+v", notice.doc != nil, notice.markup)
	}
	contains(t, "caption of a long node request", notice.doc.Caption, "a button below")
}

// Длинный текстовый запрос уходит документом, и подпись говорит тому, кто
// вправе ответить, как ответить: номер для /answer не прячется в файле.
// Viewer подсказки и в подписи не получает.
func TestLongTextRequestNamesHowToAnswerInTheCaption(t *testing.T) {
	request := actorRequestOf("text")
	request.Context = strings.Repeat("Line of the build log.\n", 700)
	f := telegramFacts{swarm: "build", actor: "builder-1", request: &request}
	question := store.TelegramNotification{ID: 7, Kind: "actor_question", Event: store.TelegramEvent{Swarm: "s", Request: "r2"}, Manager: true}
	notice := renderTelegramNotice(question, f, "https://brood.example")
	if notice.doc == nil {
		t.Fatalf("a long text request went as a message of %d characters", telegram.TextLength(notice.text))
	}
	if telegram.TextLength(notice.doc.Caption) > telegram.CaptionLimit {
		t.Fatalf("caption of %d characters", telegram.TextLength(notice.doc.Caption))
	}
	contains(t, "manager's caption", notice.doc.Caption, `Swarm "build"`, "/answer 7 <your answer>", "attached", "https://brood.example/#/swarms/s")
	// Reply на документ отвечает так же, как на сообщение: поле ответа
	// открывается и у него.
	if want := (&telegram.ForceReply{Placeholder: "Answer to request 7"}); !reflect.DeepEqual(notice.markup, want) {
		t.Fatalf("manager's long text request markup = %+v, want the reply field %+v", notice.markup, want)
	}
	question.Manager = false
	if notice := renderTelegramNotice(question, f, "https://brood.example"); notice.doc == nil || notice.markup != nil || strings.Contains(notice.doc.Caption, "/answer") {
		t.Fatalf("viewer's document offers /answer or the reply field: %+v, %+v", notice.doc, notice.markup)
	}
}

// Клавиатуру, которую Telegram отверг бы, уведомление не несёт: подпись,
// пустая после пробелов, и больше вариантов, чем кнопок в клавиатуре,
// оставляют выбор без кнопок, иначе уведомление не ушло бы вовсе. Подпись не
// UTF-8 чинится, как текст уведомления.
func TestKeyboardIsLeftOutWhenTelegramWouldRefuseIt(t *testing.T) {
	blank := actorRequestOf("choice")
	blank.Options[1].Label = " \t"
	if got := telegramRequestKeyboard(9, blank); got != nil {
		t.Fatalf("keyboard with a blank option = %+v, want none", got)
	}
	many := actorRequestOf("choice")
	many.Options = nil
	for i := range telegramButtonOptions + 1 {
		many.Options = append(many.Options, store.RequestOption{Label: "option " + strconv.Itoa(i), Outcome: "o" + strconv.Itoa(i)})
	}
	if got := telegramRequestKeyboard(9, many); got != nil {
		t.Fatalf("keyboard of %d options has %d rows, want none", len(many.Options), len(got.InlineKeyboard))
	}
	many.Options = many.Options[:telegramButtonOptions]
	if got := telegramRequestKeyboard(9, many); got == nil || len(got.InlineKeyboard) != telegramButtonOptions {
		t.Fatalf("keyboard of %d options = %+v", len(many.Options), got)
	}
	broken := actorRequestOf("choice")
	broken.Options[0].Label = "sql\xffite"
	if got := telegramRequestKeyboard(9, broken); got == nil || got.InlineKeyboard[0][0].Text != "sql?ite" {
		t.Fatalf("keyboard with a label that is not UTF-8 = %+v", got)
	}
}

// Тому, кто вправе ответить, уведомление без кнопок говорит ответить в
// brood, а не нажать кнопку, которой нет.
func TestManagerNoticeWithoutButtonsSaysToAnswerInBrood(t *testing.T) {
	node := nodeRequestFacts()
	node.request.State = "open"
	node.request.Options[0].Label = " "
	manager := store.TelegramNotification{ID: 7, Kind: "request_opened", Event: store.TelegramEvent{Swarm: "s/1", Request: "r1", Sequence: 3, Node: "approve"}, Manager: true}
	request := actorRequestOf("choice")
	request.Options[0].Label = " "
	question := store.TelegramNotification{ID: 7, Kind: "actor_question", Event: store.TelegramEvent{Swarm: "s", Request: "r2"}, Manager: true}
	for name, notice := range map[string]telegramNotice{
		"node request":   renderTelegramNotice(manager, node, ""),
		"actor question": renderTelegramNotice(question, telegramFacts{swarm: "build", actor: "builder-1", request: &request}, ""),
	} {
		if notice.markup != nil || strings.Contains(notice.text, "button below") {
			t.Errorf("manager's %s with a blank option offers buttons: %+v", name, notice)
		}
		contains(t, "manager's "+name+" with a blank option", notice.text, "in brood")
	}
}
