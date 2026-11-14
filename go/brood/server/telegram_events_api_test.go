package server

// FEATURE: Writing the Telegram subscriptions of the caller (#295, epic #278).
// SPEC: PUT /api/me/telegram/events takes exactly {"events": [...]} with an
// array of strings, the full set of subscriptions, and answers 200 with the
// profile, like the other profile writes. It acts only for the caller,
// behind the session and CSRF of every route; an anonymous caller gets 401.
// The write rechecks the session in its transaction (403 "session
// authorization changed") and writes nothing on any refusal. An unknown kind
// is refused with the store's vocabulary and without the caller's value; a
// repeated kind collapses, and the order is the vocabulary's. An empty set is
// stored as no subscriptions, not as the default of every kind. No response
// and no refusal carries the bot token, even one sent as a kind.
// IMPLEMENTATION: handleTelegramEventsPut and readTelegramEvents
// (telegram_profile_api.go); store.SetTelegramEvents checks the kinds.
// STATUS: DONE

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

const telegramEventsPath = "/api/me/telegram/events"

// putTelegramEvents записывает подписки и ждёт 200; возвращает профиль и
// сырое тело ответа.
func (h *telegramHarness) putTelegramEvents(client *http.Client, events []string) (map[string]any, string) {
	h.t.Helper()
	resp, data, raw := h.do(client, "PUT", telegramEventsPath, h.csrf(client), map[string]any{"events": events})
	expectStatus(h.t, "PUT "+telegramEventsPath, resp, data, http.StatusOK)
	return data, raw
}

// profileEvents — подписки из ответа профиля строками; не массив строк — nil.
func profileEvents(data map[string]any) []string {
	list, ok := data["events"].([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

func TestTelegramEventsPutWritesTheSubscriptions(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	// Повтор схлопывается, порядок — словаря хранилища, а не тела.
	data, _ := h.putTelegramEvents(alice, []string{"pass_finished", "request_opened", "pass_finished"})
	want := []string{"request_opened", "pass_finished"}
	if got := profileEvents(data); !slices.Equal(got, want) {
		t.Fatalf("PUT subscriptions %v, want %v", got, want)
	}
	if data["bot_username"] != "Alice_Bot" || data["own_bot"] != true || data["state"] != "unlinked" {
		t.Fatalf("PUT answers with the profile: %v", data)
	}
	_, view, _ := h.do(alice, "GET", "/api/me/telegram", "", nil)
	if got := profileEvents(view); !slices.Equal(got, want) {
		t.Fatalf("GET after PUT subscriptions %v, want %v", got, want)
	}
	if got := h.link(aliceID).Events; !slices.Equal(got, want) {
		t.Fatalf("stored subscriptions %v, want %v", got, want)
	}
	// Полный набор — замена, а не добавление к прежнему.
	data, _ = h.putTelegramEvents(alice, []string{"actor_question"})
	if got := profileEvents(data); !slices.Equal(got, []string{"actor_question"}) {
		t.Fatalf("second PUT subscriptions %v, want only actor_question", got)
	}
}

func TestTelegramEventsPutTakesEveryKindOfTheStore(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.putTelegramEvents(alice, []string{})
	all := store.TelegramEventKinds()
	reversed := slices.Clone(all)
	slices.Reverse(reversed)
	data, _ := h.putTelegramEvents(alice, reversed)
	if got := profileEvents(data); !slices.Equal(got, all) {
		t.Fatalf("PUT of every kind %v, want %v", got, all)
	}
	if got := h.link(aliceID).Events; !slices.Equal(got, all) {
		t.Fatalf("stored subscriptions %v, want %v", got, all)
	}
}

func TestTelegramEventsPutOfAnEmptySetUnsubscribesFromEverything(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	data, raw := h.putTelegramEvents(alice, []string{})
	if got := profileEvents(data); got == nil || len(got) != 0 {
		t.Fatalf("PUT of an empty set answers subscriptions %s", raw)
	}
	_, view, got := h.do(alice, "GET", "/api/me/telegram", "", nil)
	if events := profileEvents(view); events == nil || len(events) != 0 {
		t.Fatalf("GET after an empty set: %s", got)
	}
	if events := h.link(aliceID).Events; len(events) != 0 {
		t.Fatalf("stored subscriptions after an empty set: %v", events)
	}
}

func TestTelegramEventsPutRefusesAnUnknownKind(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.putTelegramEvents(alice, []string{"step_failed"})
	want := "unknown telegram event; known events: " + strings.Join(store.TelegramEventKinds(), ", ")
	resp, data, _ := h.do(alice, "PUT", telegramEventsPath, h.csrf(alice), map[string]any{"events": []string{"request_opened", "request_closed"}})
	expectStatus(t, "PUT of an unknown kind", resp, data, http.StatusBadRequest)
	expectTelegramRefusal(t, "PUT of an unknown kind", data, want)
	if got := h.link(aliceID).Events; !slices.Equal(got, []string{"step_failed"}) {
		t.Fatalf("a refused PUT changed the subscriptions: %v", got)
	}
}

func TestTelegramEventsPutTakesExactlyAnArrayOfStrings(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.putTelegramEvents(alice, []string{"step_failed"})
	const reason = `telegram events request must be exactly {"events"} with an array of strings`
	for _, body := range []string{
		`{}`,
		`{"events": null}`,
		`{"events": "request_opened"}`,
		`{"events": [1]}`,
		`{"events": ["request_opened", null]}`,
		`{"events": {"request_opened": true}}`,
		`{"events": [], "token": "x"}`,
		`{"Events": []}`,
		`{"events": [], "events": ["request_opened"]}`,
		`["request_opened"]`,
		`{"events": []} {}`,
		``,
	} {
		resp, data, _ := h.doRaw(alice, "PUT", telegramEventsPath, h.csrf(alice), body)
		expectStatus(t, "PUT of "+body, resp, data, http.StatusBadRequest)
		expectTelegramRefusal(t, "PUT of "+body, data, reason)
	}
	if got := h.link(aliceID).Events; !slices.Equal(got, []string{"step_failed"}) {
		t.Fatalf("a refused PUT changed the subscriptions: %v", got)
	}
	// Пробелы и переводы строк внутри формы её не ломают.
	resp, data, _ := h.doRaw(alice, "PUT", telegramEventsPath, h.csrf(alice), " { \"events\" : [ \"step_failed\" ,\n \"request_opened\" ] }\n")
	expectStatus(t, "PUT with whitespace", resp, data, http.StatusOK)
	if got := profileEvents(data); !slices.Equal(got, []string{"request_opened", "step_failed"}) {
		t.Fatalf("PUT with whitespace subscriptions %v", got)
	}
}

func TestTelegramEventsPutNeedsASessionAndCSRF(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	anonymous := newClient(t)
	body := map[string]any{"events": []string{}}
	resp, data, _ := h.do(anonymous, "PUT", telegramEventsPath, h.csrf(anonymous), body)
	expectStatus(t, "anonymous PUT", resp, data, http.StatusUnauthorized)
	resp, data, _ = h.do(alice, "PUT", telegramEventsPath, "", body)
	expectStatus(t, "PUT without CSRF", resp, data, http.StatusForbidden)
	if got := h.link(aliceID).Events; !slices.Equal(got, store.TelegramEventKinds()) {
		t.Fatalf("a refused PUT changed the subscriptions: %v", got)
	}
}

func TestTelegramEventsPutIsTheCallersOwn(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	bob, bobID := h.member("bob")
	h.putTelegramEvents(alice, []string{"request_opened"})
	h.putTelegramEvents(bob, []string{"actor_question"})
	if got := h.link(aliceID).Events; !slices.Equal(got, []string{"request_opened"}) {
		t.Fatalf("bob's PUT changed alice's subscriptions: %v", got)
	}
	if got := h.link(bobID).Events; !slices.Equal(got, []string{"actor_question"}) {
		t.Fatalf("bob's subscriptions: %v", got)
	}
}

// Сессию запроса отзывают между middleware и транзакцией записи: запись
// отказывает и подписок не меняет.
func TestTelegramEventsPutRechecksTheSessionInItsTransaction(t *testing.T) {
	var revoke atomic.Bool
	var h *telegramHarness
	h = startTelegramHarness(t, newTestBotAPI(t), "", "", nil, func(o *telegramOptions) {
		o.beforeWrite = func(r *http.Request) {
			if !revoke.Load() || r.URL.Path != telegramEventsPath {
				return
			}
			user, _ := auth.UserFromContext(r.Context())
			if _, err := h.srv.store.DeleteSessionsOfUser(context.Background(), user.ID); err != nil {
				t.Error(err)
			}
		}
	})
	alice, aliceID := h.member("alice")
	revoke.Store(true)
	resp, data, _ := h.do(alice, "PUT", telegramEventsPath, h.csrf(alice), map[string]any{"events": []string{"request_opened"}})
	expectStatus(t, "PUT with a revoked session", resp, data, http.StatusForbidden)
	expectTelegramRefusal(t, "PUT with a revoked session", data, "session authorization changed")
	if got := h.link(aliceID).Events; !slices.Equal(got, store.TelegramEventKinds()) {
		t.Fatalf("PUT of a revoked session wrote the subscriptions: %v", got)
	}
}

func TestTelegramEventsPutDoesNotCarryTheBotToken(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	_, written := h.putTelegramEvents(alice, []string{"request_opened"})
	// Отказ не повторяет тело: токен, присланный видом, в него не попадает.
	_, _, unknown := h.do(alice, "PUT", telegramEventsPath, h.csrf(alice), map[string]any{"events": []string{aliceBotToken}})
	_, _, malformed := h.doRaw(alice, "PUT", telegramEventsPath, h.csrf(alice), `{"events": [7]}`)
	for what, body := range map[string]string{"PUT": written, "unknown kind": unknown, "malformed body": malformed} {
		if strings.Contains(body, aliceBotSecret) || strings.Contains(body, aliceBotToken) {
			t.Fatalf("%s response carries the bot token: %s", what, body)
		}
	}
}
