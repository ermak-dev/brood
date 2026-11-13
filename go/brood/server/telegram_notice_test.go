package server

// FEATURE: Text of a Telegram notification (#282).
// SPEC: a notification names its swarm and what happened: the node and step
// of a node request, the asking actor and the kind of answer it waits for
// (stated, not asked of the reader: a viewer may not answer), who
// answered and the answer, the delivery outcome of an answer (a failed one
// says who may retry it, not asked of every reader), the pass with its
// workflow and goal, how a pass ended, the refused or unconfirmed step with
// its reason; the goal and the reason come from the step of the event. A
// request shows its question, its options and its
// material. The link opens the swarm in the panel on the configured panel
// origin: the server serves the panel at the origin root, and the panel
// routes by the URL fragment (hash router), so the link is
// <origin>/#/swarms/<id> with the ID escaped (#194); without an origin there
// is no link. A
// text within the Bot API message limit is one message; a longer one is one
// document with the whole text and a caption within the caption limit that
// keeps the head and the link. Text that is not UTF-8 is repaired, never
// refused by Telegram.

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

func nodeRequestFacts() telegramFacts {
	outcome := "success"
	return telegramFacts{swarm: "release-train", definition: "release", request: &store.SwarmRequest{
		ID: "r1", SwarmID: "s/1", Origin: "node", Kind: "choice", Question: "Ship build 42?", Context: "Report of step 3: all green.",
		Options: []store.RequestOption{{Label: "Ship", Outcome: "success"}, {Label: "Hold", Outcome: "fail"}}, Outcome: &outcome,
	}}
}

func noticeOf(t *testing.T, kind string, ev store.TelegramEvent, f telegramFacts, origin string) telegramNotice {
	t.Helper()
	notice := renderTelegramNotice(store.TelegramNotification{ID: 7, Kind: kind, Event: ev}, f, origin)
	if notice.doc == nil && (notice.text == "" || telegram.TextLength(notice.text) > telegram.MessageLimit) {
		t.Fatalf("%s: message of %d characters", kind, telegram.TextLength(notice.text))
	}
	return notice
}

func contains(t *testing.T, what, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Errorf("%s lacks %q:\n%s", what, part, text)
		}
	}
}

func TestNodeRequestNoticeCarriesQuestionOptionsMaterialAndLink(t *testing.T) {
	notice := noticeOf(t, "request_opened", store.TelegramEvent{Swarm: "s/1", Request: "r1", Sequence: 3, Node: "approve"}, nodeRequestFacts(), "https://brood.example:8443")
	contains(t, "node request", notice.text, `Swarm "release-train"`, "node approve", "step 3", "Ship build 42?", "- Ship", "- Hold", "Report of step 3: all green.",
		"https://brood.example:8443/#/swarms/s%2F1")
}

// Ссылка открывает страницу роя: сервер отдаёт панель по пути ссылки (без
// фрагмента, его браузер не шлёт), а страницу выбирает фрагмент — панель
// маршрутизирует по hash (createHashRouter, web/src/App.tsx; маршрут
// /swarms/:id, web/src/router/MainRoutes.tsx).
func TestNoticeLinkOpensTheSwarmPageOfThePanel(t *testing.T) {
	ts := newTestServer(t)
	notice := noticeOf(t, "pass_finished", store.TelegramEvent{Swarm: "s/1", Sequence: 1, Execution: "stopped"}, nodeRequestFacts(), ts.http.URL)
	_, raw, ok := strings.Cut(notice.text, "Open in brood: ")
	if !ok {
		t.Fatalf("notice has no link:\n%s", notice.text)
	}
	link, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		t.Fatalf("link %q: %v", raw, err)
	}
	if got := link.EscapedFragment(); got != "/swarms/s%2F1" {
		t.Fatalf("link %s: fragment %q, want the panel route /swarms/s%%2F1", link, got)
	}
	resp, err := http.Get(link.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	panel, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, panel) {
		t.Fatalf("GET %s: %d, the panel %t", link, resp.StatusCode, bytes.Equal(body, panel))
	}
}

func TestNoticeWithoutPanelOriginHasNoLink(t *testing.T) {
	notice := noticeOf(t, "request_opened", store.TelegramEvent{Swarm: "s/1", Request: "r1", Sequence: 3, Node: "approve"}, nodeRequestFacts(), "")
	if strings.Contains(notice.text, "swarms/") || strings.Contains(notice.text, "http") {
		t.Fatalf("notice without a panel origin links somewhere:\n%s", notice.text)
	}
}

func TestEveryKindNamesItsEvent(t *testing.T) {
	answered := "sqlite"
	actor := telegramFacts{swarm: "build", actor: "builder-1", answeredBy: "Alice", request: &store.SwarmRequest{
		ID: "r2", SwarmID: "s", Origin: "actor", Kind: "text", Question: "Which db?", Response: &answered,
	}}
	node := nodeRequestFacts()
	node.answeredBy = "bob"
	// Задача прохода и причина отказа — из шага события, а не из строки
	// очереди.
	passStep := node
	passStep.step = &store.PipelineVisit{Sequence: 4, NodeID: "approve", State: "waiting", Goal: "Ship issue #7.", Pass: 4}
	refused := node
	refused.step = &store.PipelineVisit{Sequence: 5, NodeID: "build", State: "refused", Detail: "role build has no live bound actor", Pass: 4}
	unconfirmed := node
	unconfirmed.step = &store.PipelineVisit{Sequence: 5, NodeID: "build", State: "unconfirmed", Detail: "outcome unknown", Pass: 4}
	for _, tc := range []struct {
		kind  string
		ev    store.TelegramEvent
		facts telegramFacts
		parts []string
	}{
		{"actor_question", store.TelegramEvent{Swarm: "s", Request: "r2"}, actor, []string{`Swarm "build"`, "actor builder-1", "Which db?", "a text answer"}},
		{"request_answered", store.TelegramEvent{Swarm: "s", Request: "r2"}, actor, []string{"Alice answered", "actor builder-1", "Which db?", "sqlite"}},
		{"request_answered", store.TelegramEvent{Swarm: "s/1", Request: "r1", Sequence: 3, Node: "approve"}, node, []string{"bob answered", "node approve", "Ship"}},
		{"answer_delivery", store.TelegramEvent{Swarm: "s", Request: "r2", Delivery: "delivered"}, actor, []string{"answer to actor builder-1 was delivered", "sqlite"}},
		{"answer_delivery", store.TelegramEvent{Swarm: "s", Request: "r2", Delivery: "failed"}, actor, []string{"could not be delivered"}},
		{"answer_delivery", store.TelegramEvent{Swarm: "s", Request: "r2", Delivery: "unconfirmed"}, actor, []string{"unconfirmed", "may have reached"}},
		{"pass_started", store.TelegramEvent{Swarm: "s/1", Sequence: 4, Node: "approve", Execution: "waiting"}, passStep, []string{"pass 4 started", "workflow release", "node approve", "Ship issue #7."}},
		{"pass_finished", store.TelegramEvent{Swarm: "s/1", Sequence: 4, Node: "approve", Execution: "succeeded"}, node, []string{"pass 4 succeeded"}},
		{"pass_finished", store.TelegramEvent{Swarm: "s/1", Sequence: 4, Node: "build", Execution: "failed"}, node, []string{"pass 4 failed", "node build"}},
		{"pass_finished", store.TelegramEvent{Swarm: "s/1", Sequence: 4, Execution: "stopped"}, node, []string{"pass 4 was stopped"}},
		{"step_failed", store.TelegramEvent{Swarm: "s/1", Sequence: 5, Node: "build", State: "refused"}, refused, []string{"step 5", "node build", "was refused", "role build has no live bound actor"}},
		{"step_failed", store.TelegramEvent{Swarm: "s/1", Sequence: 5, Node: "build", State: "unconfirmed"}, unconfirmed, []string{"step 5", "outcome is unknown", "outcome unknown"}},
	} {
		notice := noticeOf(t, tc.kind, tc.ev, tc.facts, "https://brood.example")
		contains(t, tc.kind, notice.text, append(tc.parts, "https://brood.example/#/swarms/")...)
	}
	for kind, want := range map[string]string{"choice": "one of the options", "confirm": "yes or no"} {
		f := actor
		request := *actor.request
		request.Kind = kind
		if kind == "choice" {
			request.Options = []store.RequestOption{{Label: "sqlite", Outcome: "sqlite"}}
		}
		f.request = &request
		contains(t, kind+" question", noticeOf(t, "actor_question", store.TelegramEvent{Swarm: "s", Request: "r2"}, f, "").text, want)
	}
}

// Вопрос актора уходит каждому читателю роя, и viewer среди них ответить не
// вправе: уведомление говорит, какого ответа ждёт актор, а не велит отвечать.
func TestActorQuestionNamesTheAnswerItWaitsForWithoutAskingTheReader(t *testing.T) {
	for kind, want := range map[string]string{
		"text":    "The actor waits for a text answer in brood.",
		"choice":  "The actor waits for one of the options in brood.",
		"confirm": "The actor waits for a yes or no in brood.",
	} {
		request := store.SwarmRequest{ID: "r2", SwarmID: "s", Origin: "actor", Kind: kind, Question: "Which db?"}
		if kind == "choice" {
			request.Options = []store.RequestOption{{Label: "sqlite", Outcome: "sqlite"}}
		}
		f := telegramFacts{swarm: "build", actor: "builder-1", request: &request}
		text := noticeOf(t, "actor_question", store.TelegramEvent{Swarm: "s", Request: "r2"}, f, "").text
		contains(t, kind+" question", text, want)
		for _, order := range []string{"Answer with", "Answer yes"} {
			if strings.Contains(text, order) {
				t.Errorf("%s question tells every reader to answer (%q):\n%s", kind, order, text)
			}
		}
	}
}

// Неудачная доставка ответа уходит каждому читателю роя, а повторить её
// вправе только тот, кто управляет роем: уведомление говорит, кто может
// повторить, а не велит повторять каждому.
func TestFailedDeliveryNamesWhoMayRetryWithoutAskingTheReader(t *testing.T) {
	answered := "sqlite"
	f := telegramFacts{swarm: "build", actor: "builder-1", request: &store.SwarmRequest{
		ID: "r2", SwarmID: "s", Origin: "actor", Kind: "text", Question: "Which db?", Response: &answered,
	}}
	text := noticeOf(t, "answer_delivery", store.TelegramEvent{Swarm: "s", Request: "r2", Delivery: "failed"}, f, "").text
	contains(t, "failed delivery", text, "could not be delivered", "anyone who manages the swarm can retry the delivery in brood")
	for _, order := range []string{"retry it", "Retry it", "; retry", ". Retry"} {
		if strings.Contains(text, order) {
			t.Errorf("failed delivery tells every reader to retry (%q):\n%s", order, text)
		}
	}
}

func TestLongMaterialGoesAsOneDocumentWithACaption(t *testing.T) {
	f := nodeRequestFacts()
	f.request.Context = strings.Repeat("Line of the step report.\n", 700)
	notice := noticeOf(t, "request_opened", store.TelegramEvent{Swarm: "s/1", Request: "r1", Sequence: 3, Node: "approve"}, f, "https://brood.example")
	if notice.doc == nil {
		t.Fatalf("a text of %d characters went as a message", telegram.TextLength(notice.text))
	}
	doc := notice.doc
	contains(t, "document", string(doc.Content), "Ship build 42?", "- Ship", f.request.Context, "https://brood.example/#/swarms/s%2F1")
	if telegram.TextLength(doc.Caption) > telegram.CaptionLimit {
		t.Fatalf("caption of %d characters", telegram.TextLength(doc.Caption))
	}
	contains(t, "caption", doc.Caption, `Swarm "release-train"`, "attached", "https://brood.example/#/swarms/s%2F1")
	if doc.Name != "brood-notification-7.txt" {
		t.Fatalf("document name %q", doc.Name)
	}
}

func TestLongHeadStillFitsTheCaption(t *testing.T) {
	f := nodeRequestFacts()
	f.swarm = strings.Repeat("𝔰", 3000)
	f.request.Question = strings.Repeat("why? ", 2000)
	notice := noticeOf(t, "request_opened", store.TelegramEvent{Swarm: "s", Request: "r1", Sequence: 3, Node: strings.Repeat("n", 2000)}, f, "https://brood.example")
	if notice.doc == nil || telegram.TextLength(notice.doc.Caption) > telegram.CaptionLimit || !utf8.ValidString(notice.doc.Caption) {
		t.Fatalf("caption of a long head: %+v", notice.doc)
	}
	contains(t, "caption", notice.doc.Caption, "https://brood.example/#/swarms/s")
}

func TestNoticeRepairsTextThatIsNotUTF8(t *testing.T) {
	f := nodeRequestFacts()
	f.request.Question = "Ship \xff?"
	notice := noticeOf(t, "request_opened", store.TelegramEvent{Swarm: "s", Request: "r1", Sequence: 3, Node: "approve"}, f, "")
	if !utf8.ValidString(notice.text) {
		t.Fatalf("notice is not UTF-8: %q", notice.text)
	}
}

func TestPanelOriginSetting(t *testing.T) {
	for raw, want := range map[string]string{
		"":                           "",
		"https://brood.example":      "https://brood.example",
		"https://brood.example/":     "https://brood.example",
		"https://brood.example:8443": "https://brood.example:8443",
		"http://127.0.0.1:8080":      "http://127.0.0.1:8080",
	} {
		cfg, err := LoadConfig(func(key string) string {
			if key == panelOriginSetting {
				return raw
			}
			return ""
		})
		if err != nil || cfg.PanelOrigin != want {
			t.Errorf("%s=%q: origin %q, %v; want %q", panelOriginSetting, raw, cfg.PanelOrigin, err, want)
		}
	}
	for _, raw := range []string{"brood.example", "ftp://brood.example", "https://user@brood.example", "https://brood.example/panel", "https://brood.example?x=1", "https://brood.example#top", "https://"} {
		_, err := LoadConfig(func(key string) string {
			if key == panelOriginSetting {
				return raw
			}
			return ""
		})
		var cfgErr *ConfigError
		if !errors.As(err, &cfgErr) || cfgErr.Setting != panelOriginSetting {
			t.Errorf("%s=%q: %v, want a refusal naming the setting", panelOriginSetting, raw, err)
		}
	}
}
