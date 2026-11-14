package server

// FEATURE: Telegram profile API of the authenticated user (#281).
// SPEC: GET/PUT/DELETE /api/me/telegram and POST /api/me/telegram/link act
// only for the caller, behind the session and CSRF of every route. PUT takes
// exactly {"token"}, proves it with getMe and stores the bot username from
// the getMe answer, never from the caller; no response carries the token,
// the link code response included.
// GET shows the bot the link uses, the state and its reason, the
// subscriptions, the linked chat and whether a link code waits; a user who
// never set anything gets the empty state. The linked chat carries the
// Telegram username of its sender from the link row (#294): the one of the
// link code, then of the latest message or button press of the chat, so it
// survives a restart; a username the store would refuse counts as none.
// DELETE removes the own bot and the chat link. A link code is server-made,
// crypto/rand of 256 bits in URL-safe base64, valid for telegramLinkCodeTTL.
// Every write rechecks the session in its transaction. Only a getMe refusal
// of the token itself (401, 404) is a rejected token; any other refusal, such
// as 429, and an answer not from the Bot API are "try again later". No code
// is issued through a stopped installation bot.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

func TestTelegramProfileOfANewUserIsTheEmptyState(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, _ := h.member("alice")
	resp, data, _ := h.do(alice, "GET", "/api/me/telegram", "", nil)
	expectStatus(t, "GET", resp, data, http.StatusOK)
	if data["bot_username"] != "" || data["own_bot"] != false || data["state"] != "unlinked" || data["chat"] != nil || data["link_code_pending"] != false {
		t.Fatalf("empty state: %v", data)
	}
	if _, ok := data["reason"]; ok {
		t.Fatalf("empty state carries a reason: %v", data)
	}
	events, _ := data["events"].([]any)
	if len(events) != len(store.TelegramEventKinds()) {
		t.Fatalf("empty state subscriptions: %v", data["events"])
	}
}

func TestTelegramProfileTokenIsWriteOnly(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	resp, data, raw := h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": aliceBotToken})
	expectStatus(t, "PUT", resp, data, http.StatusOK)
	if data["bot_username"] != "Alice_Bot" || data["own_bot"] != true || data["state"] != "unlinked" {
		t.Fatalf("PUT view: %v", data)
	}
	_, _, got := h.do(alice, "GET", "/api/me/telegram", "", nil)
	// Ответ с кодом несёт свежий секрет: проверяется всё тело, а не только
	// поля кода и ссылки.
	code, link := h.issueCodeRaw(alice)
	if code["code"] == "" || !strings.Contains(link, code["code"].(string)) {
		t.Fatalf("POST link: %s", link)
	}
	_, _, deleted := h.do(alice, "DELETE", "/api/me/telegram", h.csrf(alice), nil)
	for what, body := range map[string]string{"PUT": raw, "GET": got, "POST link": link, "DELETE": deleted} {
		if strings.Contains(body, aliceBotSecret) || strings.Contains(body, aliceBotToken) {
			t.Fatalf("%s response carries the bot token: %s", what, body)
		}
	}
	if h.link(aliceID).HasBotToken {
		t.Fatal("DELETE kept the own bot token")
	}
}

func TestTelegramProfileTakesTheBotUsernameOnlyFromGetMe(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	for _, body := range []any{
		map[string]any{"token": aliceBotToken, "bot_username": "evil_bot"},
		map[string]any{},
		map[string]any{"token": 42},
		map[string]any{"Token": aliceBotToken},
		[]any{aliceBotToken},
	} {
		resp, data, raw := h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), body)
		expectStatus(t, "PUT with a wrong body", resp, data, http.StatusBadRequest)
		expectTelegramRefusal(t, "PUT with a wrong body", data, `telegram bot request must be exactly {"token"} with a string value`)
		if strings.Contains(raw, aliceBotSecret) {
			t.Fatalf("refusal echoes the token: %s", raw)
		}
	}
	if link := h.link(aliceID); link.HasBotToken || link.BotUsername != "" {
		t.Fatalf("a refused PUT stored a bot: %+v", link)
	}
	resp, data, _ := h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": aliceBotToken})
	expectStatus(t, "PUT", resp, data, http.StatusOK)
	if link := h.link(aliceID); !link.HasBotToken || link.BotUsername != "Alice_Bot" {
		t.Fatalf("stored bot: %+v", link)
	}
}

func TestTelegramProfileRefusesATokenTelegramRejects(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	const unknown = "710000077:SENTINEL-unknown_bot-0123456789abcdef"
	resp, data, raw := h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": unknown})
	expectStatus(t, "PUT of a token Telegram rejects", resp, data, http.StatusUnprocessableEntity)
	expectTelegramRefusal(t, "PUT of a token Telegram rejects", data, "Telegram rejected the bot token")
	if strings.Contains(raw, "SENTINEL") {
		t.Fatalf("refusal: %s", raw)
	}
	resp, data, raw = h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": "SENTINEL-not-a-token"})
	expectStatus(t, "PUT of a malformed token", resp, data, http.StatusBadRequest)
	expectTelegramRefusal(t, "PUT of a malformed token", data, "invalid telegram bot token: want <bot id>:<secret> as BotFather issues it")
	if strings.Contains(raw, "SENTINEL") {
		t.Fatalf("refusal echoes the token: %s", raw)
	}
	if h.link(aliceID).HasBotToken {
		t.Fatal("a rejected token was stored")
	}
}

func TestTelegramProfileRefusesWhenTelegramIsUnreachable(t *testing.T) {
	api := newFakeBotAPI(t)
	h := startTelegramHarness(t, api, "http://127.0.0.1:1", "", nil)
	alice, aliceID := h.member("alice")
	resp, data, raw := h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": aliceBotToken})
	expectStatus(t, "PUT without Telegram", resp, data, http.StatusServiceUnavailable)
	expectTelegramRefusal(t, "PUT without Telegram", data, "Telegram did not answer getMe: try again later")
	if strings.Contains(raw, aliceBotSecret) {
		t.Fatalf("refusal carries the token: %s", raw)
	}
	if h.link(aliceID).HasBotToken {
		t.Fatal("a token getMe did not prove was stored")
	}
}

func TestTelegramProfileRefusesABotOfAnotherUser(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	bob, bobID := h.member("bob")
	h.setBot(alice, aliceID, aliceBotToken)
	resp, data, _ := h.do(bob, "PUT", "/api/me/telegram", h.csrf(bob), map[string]any{"token": aliceBotToken})
	expectStatus(t, "PUT of a taken bot", resp, data, http.StatusConflict)
	expectTelegramRefusal(t, "PUT of a taken bot", data, "telegram bot is already in use by another user")
	if h.link(bobID).HasBotToken {
		t.Fatal("a taken bot was stored for a second user")
	}
}

func TestTelegramProfileNeedsASessionAndCSRF(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	anonymous := newClient(t)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		resp, data, _ := h.do(anonymous, method, "/api/me/telegram", h.csrf(anonymous), map[string]any{"token": aliceBotToken})
		expectStatus(t, "anonymous "+method, resp, data, http.StatusUnauthorized)
	}
	resp, data, _ := h.do(anonymous, "POST", "/api/me/telegram/link", h.csrf(anonymous), nil)
	expectStatus(t, "anonymous link", resp, data, http.StatusUnauthorized)
	for _, method := range []string{"PUT", "DELETE"} {
		resp, data, _ := h.do(alice, method, "/api/me/telegram", "", map[string]any{"token": aliceBotToken})
		expectStatus(t, method+" without CSRF", resp, data, http.StatusForbidden)
	}
	resp, data, _ = h.do(alice, "POST", "/api/me/telegram/link", "", nil)
	expectStatus(t, "link without CSRF", resp, data, http.StatusForbidden)
	if link := h.link(aliceID); link.HasBotToken || link.LinkCodeExpiresAt != "" {
		t.Fatalf("a refused request wrote the link: %+v", link)
	}
}

// expectTelegramRefusal: сознательный отказ несёт публичную причину и в
// detail, и в reason (#276).
func expectTelegramRefusal(t *testing.T, what string, data map[string]any, want string) {
	t.Helper()
	if data["detail"] != want || data["reason"] != want {
		t.Fatalf("%s: refusal %v, want detail and reason %q", what, data, want)
	}
}

var linkCodeShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func TestTelegramLinkCodeIsRandomURLSafeAndExpires(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	first := h.issueCode(alice)
	code, _ := first["code"].(string)
	raw, err := base64.RawURLEncoding.DecodeString(code)
	if !linkCodeShape.MatchString(code) || err != nil || len(raw) != 32 {
		t.Fatalf("link code %q is not 32 random bytes in URL-safe base64", code)
	}
	if first["url"] != "https://t.me/Alice_Bot?start="+code || first["bot_username"] != "Alice_Bot" {
		t.Fatalf("link: %v", first)
	}
	want := h.clock.Now().Add(telegramLinkCodeTTL).UTC().Format(time.RFC3339)
	if first["expires_at"] != want || telegramLinkCodeTTL <= 0 || telegramLinkCodeTTL > time.Hour {
		t.Fatalf("link code expires at %v, want %s", first["expires_at"], want)
	}
	_, view, _ := h.do(alice, "GET", "/api/me/telegram", "", nil)
	if view["link_code_pending"] != true || view["link_code_expires_at"] != want {
		t.Fatalf("view of a waiting code: %v", view)
	}
	if second := h.issueCode(alice); second["code"] == code {
		t.Fatal("a new link code repeats the previous one")
	}
	h.clock.Advance(telegramLinkCodeTTL)
	_, view, _ = h.do(alice, "GET", "/api/me/telegram", "", nil)
	if view["link_code_pending"] != false {
		t.Fatalf("an expired code is shown waiting: %v", view)
	}
}

func TestTelegramLinkCodesDoNotRepeat(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		code, err := newTelegramLinkCode(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if !linkCodeShape.MatchString(code) || seen[code] {
			t.Fatalf("link code %q: malformed or repeated", code)
		}
		seen[code] = true
	}
}

func TestTelegramLinkCodeNeedsABot(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	resp, data, _ := h.do(alice, "POST", "/api/me/telegram/link", h.csrf(alice), nil)
	expectStatus(t, "link without a bot", resp, data, http.StatusConflict)
	expectTelegramRefusal(t, "link without a bot", data, "no telegram bot: set a bot token first")
	if h.link(aliceID).LinkCodeExpiresAt != "" {
		t.Fatal("a code was issued without a bot")
	}
}

func TestTelegramLinkCodeIsRefusedWhileLinked(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.linkChat(alice, aliceBotToken, 555, "anton")
	resp, data, _ := h.do(alice, "POST", "/api/me/telegram/link", h.csrf(alice), nil)
	expectStatus(t, "link while linked", resp, data, http.StatusConflict)
	expectTelegramRefusal(t, "link while linked", data, "telegram chat is already linked: unlink it first")
	if h.link(aliceID).State != "linked" {
		t.Fatal("a refused code changed the link")
	}
}

func TestTelegramProfileShowsTheLinkedChat(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.linkChat(alice, aliceBotToken, 555, "anton")
	_, data, _ := h.do(alice, "GET", "/api/me/telegram", "", nil)
	chat, _ := data["chat"].(map[string]any)
	if data["state"] != "linked" || chat["chat_id"] != float64(555) || chat["telegram_user_id"] != float64(555) || chat["username"] != "anton" || data["link_code_pending"] != false {
		t.Fatalf("linked view: %v", data)
	}
	// Подписки — те, что держит хранилище.
	if _, err := h.srv.store.SetTelegramEvents(context.Background(), aliceID, []string{"request_opened"}); err != nil {
		t.Fatal(err)
	}
	_, data, _ = h.do(alice, "GET", "/api/me/telegram", "", nil)
	if events, _ := data["events"].([]any); !slices.Equal(events, []any{"request_opened"}) {
		t.Fatalf("subscriptions: %v", data["events"])
	}
}

func TestTelegramProfileIsTheCallersOwn(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	bob, bobID := h.member("bob")
	h.setBot(alice, aliceID, aliceBotToken)
	_, data, _ := h.do(bob, "GET", "/api/me/telegram", "", nil)
	if data["bot_username"] != "" || data["own_bot"] != false {
		t.Fatalf("bob sees alice's bot: %v", data)
	}
	resp, data, _ := h.do(bob, "DELETE", "/api/me/telegram", h.csrf(bob), nil)
	expectStatus(t, "bob DELETE", resp, data, http.StatusOK)
	if !h.link(aliceID).HasBotToken || h.link(bobID).HasBotToken {
		t.Fatal("bob's DELETE touched alice's bot")
	}
}

// Сессию запроса, загруженную middleware, отзывают, пока getMe ещё в пути:
// запись перепроверяет её в своей транзакции и ничего не пишет.
func TestTelegramProfilePutRechecksTheSessionAfterGetMe(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.api.scriptMe(aliceBotToken, func(http.ResponseWriter) bool {
		if _, err := h.srv.store.DeleteSessionsOfUser(context.Background(), aliceID); err != nil {
			t.Error(err)
		}
		return false
	})
	resp, data, raw := h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": aliceBotToken})
	expectStatus(t, "PUT with a session revoked during getMe", resp, data, http.StatusForbidden)
	expectTelegramRefusal(t, "PUT with a session revoked during getMe", data, "session authorization changed")
	if strings.Contains(raw, aliceBotSecret) || h.link(aliceID).HasBotToken {
		t.Fatalf("PUT of a revoked session stored the bot: %s", raw)
	}
	if h.srv.telegram.running(aliceID) || !h.quiet(aliceBotToken, 200*time.Millisecond) || len(h.api.polls(aliceBotToken)) != 0 {
		t.Fatal("PUT of a revoked session started a poller")
	}
}

// Сессию отзывают между middleware и транзакцией записи DELETE и POST
// /link: запись отказывает и ничего не меняет.
func TestTelegramProfileWritesRecheckTheSessionInTheirTransaction(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		unchanged    func(store.TelegramLink) bool
	}{
		{"DELETE", "/api/me/telegram", func(l store.TelegramLink) bool { return l.HasBotToken && l.BotUsername == "Alice_Bot" }},
		{"POST", "/api/me/telegram/link", func(l store.TelegramLink) bool { return l.HasBotToken && l.LinkCodeExpiresAt == "" }},
	} {
		t.Run(tc.method, func(t *testing.T) {
			var revoke atomic.Bool
			var h *telegramHarness
			h = startTelegramHarness(t, newTestBotAPI(t), "", "", nil, func(o *telegramOptions) {
				o.beforeWrite = func(r *http.Request) {
					if !revoke.Load() || r.Method != tc.method {
						return
					}
					user, _ := auth.UserFromContext(r.Context())
					if _, err := h.srv.store.DeleteSessionsOfUser(context.Background(), user.ID); err != nil {
						t.Error(err)
					}
				}
			})
			alice, aliceID := h.member("alice")
			h.setBot(alice, aliceID, aliceBotToken)
			revoke.Store(true)
			resp, data, _ := h.do(alice, tc.method, tc.path, h.csrf(alice), nil)
			expectStatus(t, tc.method+" with a revoked session", resp, data, http.StatusForbidden)
			expectTelegramRefusal(t, tc.method+" with a revoked session", data, "session authorization changed")
			if link := h.link(aliceID); !tc.unchanged(link) {
				t.Fatalf("%s of a revoked session wrote the link: %+v", tc.method, link)
			}
		})
	}
}

// 422 — только отказ самого токена (401, 404). Прочий отказ getMe, как 429,
// о токене ничего не говорит, а 4xx не от Bot API (страница прокси) — не
// ответ Telegram: оба — 503, повторить позже.
func TestTelegramProfileTellsARejectedTokenFromARefusedGetMe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		status int
		reason string
	}{
		{"not found", func(w http.ResponseWriter) { fakeRefuse(w, http.StatusNotFound, "Not Found") },
			http.StatusUnprocessableEntity, "Telegram rejected the bot token"},
		{"rate limit", func(w http.ResponseWriter) {
			fakeRefuseRetry(w, http.StatusTooManyRequests, "Too Many Requests: retry after 5", 5)
		}, http.StatusServiceUnavailable, "Telegram refused getMe: try again later"},
		{"bad request", func(w http.ResponseWriter) { fakeRefuse(w, http.StatusBadRequest, "Bad Request") },
			http.StatusServiceUnavailable, "Telegram refused getMe: try again later"},
		{"proxy page", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html>forbidden by the proxy</html>"))
		}, http.StatusServiceUnavailable, "Telegram did not answer getMe: try again later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTelegramHarness(t, "", nil)
			alice, aliceID := h.member("alice")
			h.api.scriptMe(aliceBotToken, func(w http.ResponseWriter) bool {
				tc.answer(w)
				return true
			})
			resp, data, raw := h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": aliceBotToken})
			expectStatus(t, "PUT, getMe "+tc.name, resp, data, tc.status)
			expectTelegramRefusal(t, "PUT, getMe "+tc.name, data, tc.reason)
			if strings.Contains(raw, aliceBotSecret) || h.link(aliceID).HasBotToken {
				t.Fatalf("PUT, getMe %s: %s", tc.name, raw)
			}
		})
	}
}

// Имя бота из getMe, которое хранилище не принимает, — отказ 422 с
// публичной причиной, а не внутренний сбой.
func TestTelegramProfileRefusesABotNameTheStoreRejects(t *testing.T) {
	api := newTestBotAPI(t)
	const dashed = "710000044:SENTINEL-dashed_bot-0123456789abcdefgh"
	api.addBot(dashed, "dashed-bot")
	h := startTelegramHarness(t, api, "", "", nil)
	alice, aliceID := h.member("alice")
	resp, data, raw := h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": dashed})
	expectStatus(t, "PUT of a bot with an unusable name", resp, data, http.StatusUnprocessableEntity)
	expectTelegramRefusal(t, "PUT of a bot with an unusable name", data, "Telegram named the bot with an unusable username")
	if strings.Contains(raw, "SENTINEL") || h.link(aliceID).HasBotToken {
		t.Fatalf("PUT of a bot with an unusable name: %s", raw)
	}
}

// Код — ровно байты источника случайности сервера в base64url: выдача не
// подменяет его своим счётчиком.
func TestTelegramLinkCodeComesFromTheServerRandomSource(t *testing.T) {
	source := make([]byte, telegramLinkCodeBytes)
	for i := range source {
		source[i] = byte(7*i + 3)
	}
	h := startTelegramHarness(t, newTestBotAPI(t), "", "", nil, func(o *telegramOptions) { o.random = bytes.NewReader(source) })
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	if code := h.issueCode(alice)["code"]; code != base64.RawURLEncoding.EncodeToString(source) {
		t.Fatalf("link code %v is not the server's random bytes", code)
	}
}

func TestTelegramLinkCodeIsRefusedWhileTheInstallationBotIsStopped(t *testing.T) {
	h := newTelegramHarness(t, installBotToken, nil)
	bob, bobID := h.member("bob")
	h.api.script(installBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		fakeRefuse(w, http.StatusUnauthorized, "Unauthorized")
		return true
	})
	telegramEventually(t, "the installation bot stops", func() bool {
		_, failure := h.srv.telegram.installation()
		return failure != ""
	})
	_, failure := h.srv.telegram.installation()
	resp, data, raw := h.do(bob, "POST", "/api/me/telegram/link", h.csrf(bob), nil)
	expectStatus(t, "link through a stopped installation bot", resp, data, http.StatusConflict)
	expectTelegramRefusal(t, "link through a stopped installation bot", data, "the installation telegram bot is stopped: "+failure)
	if strings.Contains(raw, installSecret) || h.link(bobID).LinkCodeExpiresAt != "" {
		t.Fatalf("a code was issued through a stopped installation bot: %s", raw)
	}
}

// Профиль показывает имя, которое собеседник привязанного чата носил в
// последнем сообщении или нажатии кнопки, из строки привязки; собеседник,
// снявший имя, остаётся без имени; DELETE имя снимает.
func TestTelegramProfileShowsTheLatestUsernameOfTheLinkedChat(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.linkChat(alice, aliceBotToken, 555, "anton")
	username := func() any {
		_, data, _ := h.do(alice, "GET", "/api/me/telegram", "", nil)
		chat, _ := data["chat"].(map[string]any)
		return chat["username"]
	}
	h.deliver(aliceBotToken, privateMessage(555, "anton_renamed", "hello"))
	if got := username(); got != "anton_renamed" || h.link(aliceID).TelegramUsername != "anton_renamed" {
		t.Fatalf("linked chat after a rename: %v, link %+v", got, h.link(aliceID))
	}
	// Нажатие кнопки ответа и кнопки подтверждения несёт того же собеседника.
	for i, data := range []string{"stale", telegramConfirmPrefix + "stale"} {
		name := fmt.Sprintf("anton_pressed_%d", i)
		press := callbackPress(fmt.Sprintf("press-%d", i), 555, 1, data)
		press["callback_query"].(map[string]any)["from"].(map[string]any)["username"] = name
		h.deliver(aliceBotToken, press)
		if got := username(); got != name {
			t.Fatalf("linked chat after a press of %q: %v", data, got)
		}
	}
	h.deliver(aliceBotToken, privateMessage(555, "", "hello"))
	if got := username(); got != nil || h.link(aliceID).TelegramUsername != "" {
		t.Fatalf("linked chat after the username was removed: %v", got)
	}
	h.deliver(aliceBotToken, privateMessage(555, "anton", "hello"))
	resp, data, _ := h.do(alice, "DELETE", "/api/me/telegram", h.csrf(alice), nil)
	expectStatus(t, "DELETE", resp, data, http.StatusOK)
	if name := h.link(aliceID).TelegramUsername; name != "" {
		t.Fatalf("DELETE kept the Telegram username %q of the unlinked chat", name)
	}
}

// Имя собеседника лежит в строке привязки: после перезапуска сервера профиль
// показывает его, хотя чат боту больше не писал.
func TestTelegramProfileUsernameSurvivesARestart(t *testing.T) {
	dbPath := t.TempDir() + "/brood.db"
	api := newTestBotAPI(t)
	first := startTelegramHarnessAt(t, dbPath, api, "", "", nil)
	alice, aliceID := first.member("alice")
	first.setBot(alice, aliceID, aliceBotToken)
	first.linkChat(alice, aliceBotToken, 555, "anton")
	first.http.Close()
	if err := first.srv.Close(); err != nil {
		t.Fatal(err)
	}
	h := startTelegramHarnessAt(t, dbPath, api, "", "", nil)
	_, data, _ := h.do(h.signIn("alice"), "GET", "/api/me/telegram", "", nil)
	if chat, _ := data["chat"].(map[string]any); data["state"] != "linked" || chat["username"] != "anton" {
		t.Fatalf("linked chat after a restart: %v", data)
	}
}

// Имя, которое хранилище не примет, привязку не срывает: чат привязан без
// имени, а следующее сообщение с годным именем его приносит.
func TestTelegramLinkTakesAMalformedUsernameAsNone(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.linkChat(alice, aliceBotToken, 555, "anton-dashed")
	if link := h.link(aliceID); link.State != "linked" || link.TelegramUsername != "" {
		t.Fatalf("link of a sender with a malformed username = %+v", link)
	}
	if sent := h.api.sentTo(aliceBotToken, 555); len(sent) != 1 || sent[0] != telegramReplyLinked {
		t.Fatalf("replies to the link = %q, want the linked reply", sent)
	}
	h.deliver(aliceBotToken, privateMessage(555, "anton", "hello"))
	if link := h.link(aliceID); link.TelegramUsername != "anton" {
		t.Fatalf("link after a usable username = %+v", link)
	}
}
