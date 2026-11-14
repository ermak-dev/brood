package telegram

// FEATURE: Telegram Bot API client on net/http (#280).
// SPEC: getMe, getUpdates, sendMessage, sendDocument, editMessageReplyMarkup
// and answerCallbackQuery reach <base>/bot<token>/<method> by POST, encode
// their parameters as the Bot API documents them and decode only the fields
// brood uses. sendMessage and sendDocument carry an inline keyboard or a
// force reply (#298), editMessageReplyMarkup only an inline keyboard; a nil
// markup of either kind is none. sendMessage turns link previews off:
// Telegram would fetch the URLs of mirrored text, tokens in them included.
// The bot token is an argument of NewClient apart from Config; the base URL
// is a setting with the public API as default; the transport takes no proxy
// from the environment. A local refusal never reaches the server.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	botToken    = "123456789:SENTINEL-tg_token-0123456789abcdef"
	tokenSecret = "SENTINEL-tg_token-0123456789abcdef"
)

type request struct {
	method      string
	contentType string
	body        []byte
	// conn is the client address of the connection the request came on.
	conn string
	// gone closes when the client drops the request.
	gone <-chan struct{}
}

// fakeBot starts a fake Bot API server and a client of the test bot on it.
func fakeBot(t *testing.T, answer func(w http.ResponseWriter, r request)) (*Client, *atomic.Int32) {
	t.Helper()
	base, hits := serve(t, answer)
	return newClient(t, Config{BaseURL: base}), hits
}

// serve starts a fake Bot API server and returns its base URL: every POST to
// a method of the test bot goes to answer, anything else is a test failure.
func serve(t *testing.T, answer func(w http.ResponseWriter, r request)) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		method, ok := strings.CutPrefix(r.URL.Path, "/bot"+botToken+"/")
		if !ok || r.Method != http.MethodPost || r.URL.RawQuery != "" {
			t.Errorf("request outside the bot methods: %s", r.Method)
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		answer(w, request{method: method, contentType: r.Header.Get("Content-Type"), body: body, conn: r.RemoteAddr, gone: r.Context().Done()})
	}))
	t.Cleanup(s.Close)
	return s.URL, &hits
}

func newClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := NewClient(botToken, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func result(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": v})
}

// sameJSON compares a JSON request body with the expected document.
func sameJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Errorf("request body is not JSON: %q", got)
		return
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad expectation %q", want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("request body %s, want %s", got, want)
	}
}

func expect(t *testing.T, r request, method string) {
	t.Helper()
	if r.method != method {
		t.Errorf("method %q, want %q", r.method, method)
	}
}

func message(id int64, text string) map[string]any {
	return map[string]any{"message_id": id, "chat": map[string]any{"id": 777, "type": "private", "first_name": "Anton"}, "date": 1763000000, "text": text}
}

var keyboard = &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: "Yes", CallbackData: "r:1:yes"}, {Text: "No", CallbackData: "r:1:no"}}}}

const keyboardJSON = `{"inline_keyboard":[[{"text":"Yes","callback_data":"r:1:yes"},{"text":"No","callback_data":"r:1:no"}]]}`

func TestNewClientValidatesSettings(t *testing.T) {
	c := newClient(t, Config{})
	if c.base != DefaultBaseURL || DefaultBaseURL != "https://api.telegram.org" {
		t.Fatalf("default base URL %q", c.base)
	}
	for _, base := range []string{"https://api.telegram.org", "https://api.telegram.org/", "https://tg.example:8443", "http://127.0.0.1:8081", "http://localhost:8081", "http://[::1]:8081"} {
		if _, err := NewClient(botToken, Config{BaseURL: base}); err != nil {
			t.Errorf("refused base URL %q: %v", base, err)
		}
	}
	for _, base := range []string{"http://api.telegram.org", "http://10.0.0.1:8081", "ftp://api.telegram.org", "api.telegram.org", "https://", "https://user:pw@api.telegram.org", "https://api.telegram.org/relay", "https://api.telegram.org?x=1", "https://api.telegram.org/#f", "https://api.telegram.org:99999", "https://api.telegram.org\n"} {
		if _, err := NewClient(botToken, Config{BaseURL: base}); err == nil {
			t.Errorf("accepted base URL %q", base)
		}
	}
	for _, token := range []string{"", "123456", ":abcdef", "abc:def", "123:abc/def", "123:abc?x=1", "123:abc def", "123:abc\n", "123:" + strings.Repeat("a", 200)} {
		_, err := NewClient(token, Config{})
		if err == nil {
			t.Errorf("accepted token %q", token)
			continue
		}
		if secret := token[strings.Index(token, ":")+1:]; len(secret) > 3 && strings.Contains(err.Error(), secret) {
			t.Errorf("refusal shows the token: %v", err)
		}
	}
	for _, timeout := range []time.Duration{-time.Second, MaxTimeout + time.Second} {
		if _, err := NewClient(botToken, Config{Timeout: timeout}); err == nil {
			t.Errorf("accepted timeout %s", timeout)
		}
	}
	if _, err := NewClient(botToken, Config{Timeout: MaxTimeout}); err != nil {
		t.Errorf("refused the maximal timeout: %v", err)
	}
}

// The client takes no proxy from HTTPS_PROXY and the like: a proxy would
// carry every request, token in its path, through a host no setting names.
func TestTransportTakesNoProxyFromTheEnvironment(t *testing.T) {
	c := newClient(t, Config{})
	if c.http.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("the transport has a proxy function")
	}
}

func TestGetMe(t *testing.T) {
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) {
		expect(t, r, "getMe")
		result(w, map[string]any{"id": 42, "is_bot": true, "first_name": "Brood", "username": "brood_bot", "can_join_groups": false, "supports_inline_queries": false})
	})
	me, err := c.GetMe(context.Background())
	if err != nil || me != (User{ID: 42, IsBot: true, FirstName: "Brood", Username: "brood_bot"}) {
		t.Fatalf("GetMe = %+v, %v", me, err)
	}
}

func TestGetMeRequiresABotWithUsername(t *testing.T) {
	for _, bad := range []map[string]any{{"id": 42, "is_bot": false, "username": "person"}, {"id": 42, "is_bot": true}, {"is_bot": true, "username": "brood_bot"}} {
		c, _ := fakeBot(t, func(w http.ResponseWriter, r request) { result(w, bad) })
		me, err := c.GetMe(context.Background())
		if !errors.Is(err, ErrUncertain) || me != (User{}) {
			t.Errorf("GetMe on %v = %+v, %v", bad, me, err)
		}
	}
}

func TestSendMessage(t *testing.T) {
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) {
		expect(t, r, "sendMessage")
		if r.contentType != "application/json" {
			t.Errorf("content type %q", r.contentType)
		}
		sameJSON(t, r.body, `{"chat_id":777,"text":"Approve the plan?","link_preview_options":{"is_disabled":true},"reply_markup":`+keyboardJSON+`}`)
		m := message(9, "Approve the plan?")
		m["reply_markup"] = json.RawMessage(keyboardJSON)
		result(w, m)
	})
	m, err := c.SendMessage(context.Background(), 777, "Approve the plan?", keyboard)
	if err != nil || m.MessageID != 9 || m.Chat != (Chat{ID: 777, Type: ChatPrivate}) || m.Text != "Approve the plan?" {
		t.Fatalf("SendMessage = %+v, %v", m, err)
	}
}

func TestSendMessageWithoutKeyboard(t *testing.T) {
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) {
		sameJSON(t, r.body, `{"chat_id":-100123,"text":"Swarm started.","link_preview_options":{"is_disabled":true}}`)
		result(w, message(10, "Swarm started."))
	})
	if m, err := c.SendMessage(context.Background(), -100123, "Swarm started.", nil); err != nil || m.MessageID != 10 {
		t.Fatalf("SendMessage = %+v, %v", m, err)
	}
}

func TestSendMessageAcceptsTheLimitInUTF16Units(t *testing.T) {
	text := strings.Repeat(astral, MessageLimit/2)
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) { result(w, message(11, text)) })
	if _, err := c.SendMessage(context.Background(), 777, text, nil); err != nil || hits.Load() != 1 {
		t.Fatalf("SendMessage of %d units: %v", TextLength(text), err)
	}
}

// localRefusal demands an ErrInvalid that is neither a Telegram refusal nor
// an uncertain outcome.
func localRefusal(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrRefused) || errors.Is(err, ErrUncertain) {
		t.Errorf("%s: %v, want a local refusal", name, err)
	}
}

// buttonRow returns a row of n buttons that send data.
func buttonRow(n int, data string) []InlineKeyboardButton {
	out := make([]InlineKeyboardButton, n)
	for i := range out {
		out[i] = InlineKeyboardButton{Text: "b", CallbackData: data}
	}
	return out
}

// keyboardOf returns a keyboard of rows.
func keyboardOf(rows ...[]InlineKeyboardButton) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

func TestSendMessageRefusesLocally(t *testing.T) {
	cases := []struct {
		name   string
		chat   int64
		text   string
		markup *InlineKeyboardMarkup
	}{
		{"no chat", 0, "hi", nil},
		{"empty text", 777, "", nil},
		{"blank text", 777, " \n\t", nil},
		{"text over the limit", 777, strings.Repeat("a", MessageLimit+1), nil},
		{"surrogate pairs over the limit", 777, strings.Repeat(astral, MessageLimit/2) + "a", nil},
		{"text not UTF-8", 777, "\xff", nil},
		{"keyboard without rows", 777, "hi", &InlineKeyboardMarkup{}},
		{"keyboard of zero rows", 777, "hi", &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}},
		{"empty row", 777, "hi", &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{}}}},
		{"blank button", 777, "hi", &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: " ", CallbackData: "d"}}}}},
		{"no callback data", 777, "hi", &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: "b"}}}}},
		{"callback data over 64 bytes", 777, "hi", keyboardOf(buttonRow(1, strings.Repeat("d", 65)))},
		{"callback data of 33 characters in 66 bytes", 777, "hi", keyboardOf(buttonRow(1, strings.Repeat("я", 33)))},
		{"101 buttons", 777, "hi", keyboardOf(buttonRow(50, "d"), buttonRow(51, "d"))},
	}
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) { result(w, message(1, "x")) })
	for _, tc := range cases {
		_, err := c.SendMessage(context.Background(), tc.chat, tc.text, tc.markup)
		localRefusal(t, tc.name, err)
	}
	if hits.Load() != 0 {
		t.Fatalf("local refusals reached the server %d times", hits.Load())
	}
}

// The Bot API limits are spelled out, not taken from the package constants:
// the tests pin the documented values.
func TestSendMessageAcceptsKeyboardLimits(t *testing.T) {
	cases := map[string]*InlineKeyboardMarkup{
		"100 buttons":                                keyboardOf(buttonRow(50, "d"), buttonRow(50, "d")),
		"100 buttons in one row":                     keyboardOf(buttonRow(100, "d")),
		"64 bytes of callback data":                  keyboardOf(buttonRow(1, strings.Repeat("d", 64))),
		"callback data of 32 characters in 64 bytes": keyboardOf(buttonRow(1, strings.Repeat("я", 32))),
	}
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) { result(w, message(1, "x")) })
	for name, markup := range cases {
		if _, err := c.SendMessage(context.Background(), 777, "hi", markup); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if int(hits.Load()) != len(cases) {
		t.Fatalf("server saw %d requests, want %d", hits.Load(), len(cases))
	}
}

const forceReplyJSON = `{"force_reply":true,"input_field_placeholder":"Your answer"}`

// A force reply asks the Telegram client to open a reply to the message: the
// Bot API takes force_reply true and an optional input field placeholder.
func TestSendMessageWithForceReply(t *testing.T) {
	var want string
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) {
		expect(t, r, "sendMessage")
		sameJSON(t, r.body, want)
		result(w, message(14, "Which database?"))
	})
	for _, tc := range []struct {
		reply *ForceReply
		json  string
	}{
		{&ForceReply{Placeholder: "Your answer"}, forceReplyJSON},
		{&ForceReply{}, `{"force_reply":true}`},
	} {
		want = `{"chat_id":777,"text":"Which database?","link_preview_options":{"is_disabled":true},"reply_markup":` + tc.json + `}`
		if m, err := c.SendMessage(context.Background(), 777, "Which database?", tc.reply); err != nil || m.MessageID != 14 {
			t.Fatalf("SendMessage with %+v = %+v, %v", tc.reply, m, err)
		}
	}
}

func TestSendDocumentWithForceReply(t *testing.T) {
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) {
		expect(t, r, "sendDocument")
		fields, _, _ := multipartFields(t, r)
		sameJSON(t, []byte(fields["reply_markup"]), forceReplyJSON)
		result(w, message(16, ""))
	})
	m, err := c.SendDocument(context.Background(), 777, Document{Name: "request-16.txt", Content: []byte("material"), Caption: "Which database?"}, &ForceReply{Placeholder: "Your answer"})
	if err != nil || m.MessageID != 16 {
		t.Fatalf("SendDocument = %+v, %v", m, err)
	}
}

// A nil pointer of either kind is no markup: a caller passes the keyboard it
// built, nil when it built none, and the message goes without reply_markup.
func TestNilMarkupOfEitherKindIsNoMarkup(t *testing.T) {
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) {
		switch r.method {
		case "sendMessage":
			sameJSON(t, r.body, `{"chat_id":777,"text":"hi","link_preview_options":{"is_disabled":true}}`)
		case "sendDocument":
			if fields, _, _ := multipartFields(t, r); !reflect.DeepEqual(fields, map[string]string{"chat_id": "777"}) {
				t.Errorf("fields %v", fields)
			}
		}
		result(w, message(15, "hi"))
	})
	var noKeyboard *InlineKeyboardMarkup
	var noReply *ForceReply
	for name, markup := range map[string]ReplyMarkup{"nil keyboard": noKeyboard, "nil force reply": noReply} {
		if _, err := c.SendMessage(context.Background(), 777, "hi", markup); err != nil {
			t.Errorf("SendMessage with a %s: %v", name, err)
		}
		if _, err := c.SendDocument(context.Background(), 777, Document{Name: "report.txt", Content: []byte("report")}, markup); err != nil {
			t.Errorf("SendDocument with a %s: %v", name, err)
		}
	}
	if hits.Load() != 4 {
		t.Fatalf("server saw %d requests, want 4", hits.Load())
	}
}

// The placeholder is 1 to 64 characters of UTF-8, counted as the Bot API
// counts them (TextLength); a bad one never reaches the server. The limit is
// spelled out, not taken from the package constants.
func TestForceReplyPlaceholderLimits(t *testing.T) {
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) { result(w, message(1, "x")) })
	doc := Document{Name: "request.txt", Content: []byte("material")}
	for name, placeholder := range map[string]string{
		"blank placeholder":                  " \t",
		"placeholder over 64 characters":     strings.Repeat("p", 65),
		"surrogate pairs over 64 characters": strings.Repeat(astral, 32) + "p",
		"placeholder not UTF-8":              "\xff",
	} {
		_, err := c.SendMessage(context.Background(), 777, "hi", &ForceReply{Placeholder: placeholder})
		localRefusal(t, "message with a "+name, err)
		_, err = c.SendDocument(context.Background(), 777, doc, &ForceReply{Placeholder: placeholder})
		localRefusal(t, "document with a "+name, err)
	}
	if hits.Load() != 0 {
		t.Fatalf("local refusals reached the server %d times", hits.Load())
	}
	for name, placeholder := range map[string]string{
		"64 characters":      strings.Repeat("p", 64),
		"32 surrogate pairs": strings.Repeat(astral, 32),
	} {
		if _, err := c.SendMessage(context.Background(), 777, "hi", &ForceReply{Placeholder: placeholder}); err != nil {
			t.Errorf("placeholder of %s: %v", name, err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("server saw %d requests, want 2", hits.Load())
	}
}

// multipartFields reads a multipart/form-data body: fields by name, the
// document part by its form name.
func multipartFields(t *testing.T, r request) (fields map[string]string, filename string, content []byte) {
	t.Helper()
	kind, params, err := mime.ParseMediaType(r.contentType)
	if err != nil || kind != "multipart/form-data" {
		t.Fatalf("content type %q", r.contentType)
	}
	fields = map[string]string{}
	mr := multipart.NewReader(bytes.NewReader(r.body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return fields, filename, content
		}
		if err != nil {
			t.Fatalf("multipart: %v", err)
		}
		b, _ := io.ReadAll(part)
		if part.FormName() == "document" {
			filename, content = part.FileName(), b
			continue
		}
		fields[part.FormName()] = string(b)
	}
}

func TestSendDocument(t *testing.T) {
	material := []byte(strings.Repeat("Design notes of the request.\n", 400))
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) {
		expect(t, r, "sendDocument")
		fields, name, content := multipartFields(t, r)
		if !reflect.DeepEqual(fields, map[string]string{"chat_id": "777", "caption": "Request 12: approve the design?", "reply_markup": keyboardJSON}) {
			t.Errorf("fields %v", fields)
		}
		if name != "request-12.txt" || !bytes.Equal(content, material) {
			t.Errorf("document %q of %d bytes", name, len(content))
		}
		m := message(12, "")
		m["document"] = map[string]any{"file_id": "f", "file_unique_id": "u", "file_name": name}
		result(w, m)
	})
	m, err := c.SendDocument(context.Background(), 777, Document{Name: "request-12.txt", Content: material, Caption: "Request 12: approve the design?"}, keyboard)
	if err != nil || m.MessageID != 12 {
		t.Fatalf("SendDocument = %+v, %v", m, err)
	}
}

func TestSendDocumentWithoutCaptionOrKeyboard(t *testing.T) {
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) {
		fields, name, content := multipartFields(t, r)
		if !reflect.DeepEqual(fields, map[string]string{"chat_id": "777"}) || name != "report.txt" || string(content) != "report" {
			t.Errorf("fields %v, document %q %q", fields, name, content)
		}
		result(w, message(13, ""))
	})
	m, err := c.SendDocument(context.Background(), 777, Document{Name: "report.txt", Content: []byte("report")}, nil)
	if err != nil || m.MessageID != 13 || hits.Load() != 1 {
		t.Fatalf("SendDocument = %+v, %v after %d requests", m, err, hits.Load())
	}
}

func TestSendDocumentRefusesLocally(t *testing.T) {
	ok := Document{Name: "request.txt", Content: []byte("material")}
	with := func(change func(*Document)) Document { d := ok; change(&d); return d }
	cases := map[string]Document{
		"no name":          with(func(d *Document) { d.Name = "" }),
		"path in the name": with(func(d *Document) { d.Name = "../request.txt" }),
		"space in name":    with(func(d *Document) { d.Name = "a b.txt" }),
		"quote in name":    with(func(d *Document) { d.Name = `a".txt` }),
		"empty content":    with(func(d *Document) { d.Content = nil }),
		"content too big":  with(func(d *Document) { d.Content = make([]byte, MaxDocumentBytes+1) }),
		"caption too long": with(func(d *Document) { d.Caption = strings.Repeat("c", CaptionLimit+1) }),
		"blank caption":    with(func(d *Document) { d.Caption = " " }),
	}
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) { result(w, message(1, "")) })
	for name, doc := range cases {
		_, err := c.SendDocument(context.Background(), 777, doc, nil)
		localRefusal(t, name, err)
	}
	_, err := c.SendDocument(context.Background(), 0, ok, nil)
	localRefusal(t, "no chat", err)
	_, err = c.SendDocument(context.Background(), 777, ok, &InlineKeyboardMarkup{})
	localRefusal(t, "keyboard without rows", err)
	if hits.Load() != 0 {
		t.Fatalf("local refusals reached the server %d times", hits.Load())
	}
	if _, err := c.SendDocument(context.Background(), 777, with(func(d *Document) { d.Content = make([]byte, MaxDocumentBytes) }), nil); err != nil {
		t.Fatalf("document at the size limit: %v", err)
	}
}

func TestEditMessageReplyMarkup(t *testing.T) {
	var want string
	var answer any
	c, _ := fakeBot(t, func(w http.ResponseWriter, r request) {
		expect(t, r, "editMessageReplyMarkup")
		sameJSON(t, r.body, want)
		result(w, answer)
	})
	want, answer = `{"chat_id":777,"message_id":9,"reply_markup":`+keyboardJSON+`}`, message(9, "Approve?")
	if err := c.EditMessageReplyMarkup(context.Background(), 777, 9, keyboard); err != nil {
		t.Fatal(err)
	}
	// Без клавиатуры правка снимает её с сообщения.
	want, answer = `{"chat_id":777,"message_id":9}`, true
	if err := c.EditMessageReplyMarkup(context.Background(), 777, 9, nil); err != nil {
		t.Fatal(err)
	}
	want, answer = `{"chat_id":777,"message_id":9}`, false
	if err := c.EditMessageReplyMarkup(context.Background(), 777, 9, nil); !errors.Is(err, ErrUncertain) {
		t.Fatalf("false result: %v", err)
	}
	// Пустую клавиатуру снимает nil; {"inline_keyboard":null} Telegram отвергает.
	localRefusal(t, "keyboard without rows", c.EditMessageReplyMarkup(context.Background(), 777, 9, &InlineKeyboardMarkup{}))
	localRefusal(t, "keyboard of zero rows", c.EditMessageReplyMarkup(context.Background(), 777, 9, &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}))
	localRefusal(t, "no message", c.EditMessageReplyMarkup(context.Background(), 777, 0, nil))
	localRefusal(t, "no chat", c.EditMessageReplyMarkup(context.Background(), 0, 9, nil))
}

func TestAnswerCallbackQuery(t *testing.T) {
	var want string
	c, hits := fakeBot(t, func(w http.ResponseWriter, r request) {
		expect(t, r, "answerCallbackQuery")
		sameJSON(t, r.body, want)
		result(w, true)
	})
	want = `{"callback_query_id":"4382bfdwdsb323b2d9","text":"Answer recorded."}`
	if err := c.AnswerCallbackQuery(context.Background(), "4382bfdwdsb323b2d9", "Answer recorded."); err != nil {
		t.Fatal(err)
	}
	want = `{"callback_query_id":"4382bfdwdsb323b2d9"}`
	if err := c.AnswerCallbackQuery(context.Background(), "4382bfdwdsb323b2d9", ""); err != nil {
		t.Fatal(err)
	}
	// Предел ответа -- 200 символов Bot API, в UTF-16 единицах.
	for _, text := range []string{strings.Repeat("t", 200), strings.Repeat(astral, 100)} {
		want = `{"callback_query_id":"q","text":"` + text + `"}`
		if err := c.AnswerCallbackQuery(context.Background(), "q", text); err != nil {
			t.Errorf("answer text of %d units: %v", TextLength(text), err)
		}
	}
	// ID запроса -- строка до 256 байт.
	want = `{"callback_query_id":"` + strings.Repeat("q", 256) + `"}`
	if err := c.AnswerCallbackQuery(context.Background(), strings.Repeat("q", 256), ""); err != nil {
		t.Errorf("query ID of 256 bytes: %v", err)
	}
	localRefusal(t, "no query", c.AnswerCallbackQuery(context.Background(), "", "x"))
	localRefusal(t, "query ID of 257 bytes", c.AnswerCallbackQuery(context.Background(), strings.Repeat("q", 257), ""))
	localRefusal(t, "query ID not UTF-8", c.AnswerCallbackQuery(context.Background(), "q\xff", ""))
	localRefusal(t, "text too long", c.AnswerCallbackQuery(context.Background(), "q", strings.Repeat("t", 201)))
	localRefusal(t, "surrogate pairs too long", c.AnswerCallbackQuery(context.Background(), "q", strings.Repeat(astral, 100)+"t"))
	if hits.Load() != 5 {
		t.Fatalf("server saw %d requests, want 5", hits.Load())
	}
}
