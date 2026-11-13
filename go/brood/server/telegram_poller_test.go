package server

// FEATURE: Telegram pollers of the server (#281).
// SPEC: The server long-polls getUpdates for every own bot of an enabled user
// whose link is not failed, and for the installation bot; pollers start at
// server start and when a token is saved, and stop on unlink, a disabled
// user and a revoked session. /start <code> from a private chat confirms
// the link through the store; a wrong, malformed or expired code gets one
// refusal text that names no account. Once a bot's chat is linked, updates
// from other chats and every group are ignored without a reply. The linked
// chat gets a short help text for unknown commands. 401 and 409 from
// Telegram move the own link to error with a reason that carries no token,
// and stop the poller; it stays stopped until a new token or code even when
// the error cannot be written, and a refusal of a token already replaced
// spares the new one. A batch over the client's size bound is asked again
// from the same offset with Limit 1; a batch below the offset is handled once
// and polling goes on past it; an update whose handling a stop interrupted is
// asked again. A 429 pauses at least its retry_after. A new token replaces the
// poller of the old bot; an own bot that is the installation bot gets no
// second poller. DELETE unlinks a chat of the installation bot too. A chat the
// pollers hold as linked is rechecked by the store on every message, so a
// revocation silences it before the next reconcile. A write reconciles the
// pollers before it answers, also when its client left after the commit;
// when the reconcile fails, PUT and POST /link are an internal error and
// DELETE still stops the poller of the removed bot. Saving the same token
// again restarts a poller halted by 409. Server.Close returns only after the
// pollers stopped, before the store closes. getMe of the installation bot is
// asked once when it answers, and after a failure again only past the
// backoff.

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/store"
)

func TestTelegramPollersStartAtServerStart(t *testing.T) {
	var aliceID, bobID string
	h := newTelegramHarness(t, "", func(ctx context.Context, st *store.Store) {
		aliceID = createTelegramUser(t, ctx, st, "alice")
		bobID = createTelegramUser(t, ctx, st, "bob")
		if _, err := st.SetTelegramBot(ctx, aliceID, aliceBotToken, "Alice_Bot", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetTelegramBot(ctx, bobID, bobBotToken, "bob_bot", ""); err != nil {
			t.Fatal(err)
		}
		if err := st.SetUserDisabled(ctx, bobID, true); err != nil {
			t.Fatal(err)
		}
	})
	telegramEventually(t, "the poller of an existing bot polls", func() bool { return len(h.api.polls(aliceBotToken)) > 0 })
	if !h.srv.telegram.running(aliceID) {
		t.Fatal("the poller of alice's bot is not running")
	}
	if h.srv.telegram.running(bobID) || !h.quiet(bobBotToken, 200*time.Millisecond) || len(h.api.polls(bobBotToken)) != 0 {
		t.Fatal("a disabled user's bot is polled")
	}
}

func TestTelegramPollerStartsWhenATokenIsSaved(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	if h.srv.telegram.running(aliceID) {
		t.Fatal("a poller runs before a token is saved")
	}
	resp, data := h.answersAfterSync(alice, "PUT", "/api/me/telegram", map[string]any{"token": aliceBotToken})
	expectStatus(t, "PUT", resp, data, http.StatusOK)
	if !h.srv.telegram.running(aliceID) {
		t.Fatal("PUT returned before the poller started")
	}
	telegramEventually(t, "the new bot is polled", func() bool { return len(h.api.polls(aliceBotToken)) > 0 })
	if p := h.api.polls(aliceBotToken)[0]; p.Timeout != 1 || p.Limit != 100 {
		t.Fatalf("first poll %+v, want a long poll of the harness wait", p)
	}
}

func TestTelegramStartWithTheCodeLinksThePrivateChat(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	code := h.issueCode(alice)["code"].(string)
	h.deliver(aliceBotToken, privateMessage(555, "anton", "/start "+code))
	link := h.link(aliceID)
	if link.State != "linked" || link.ChatID != 555 || link.TelegramUserID != 555 {
		t.Fatalf("link after /start: %+v", link)
	}
	if got := h.api.sentTo(aliceBotToken, 555); !slices.Equal(got, []string{telegramReplyLinked}) {
		t.Fatalf("replies to the linked chat: %q", got)
	}
	// Код одноразовый: повтор из того же чата только напоминает о привязке.
	h.deliver(aliceBotToken, privateMessage(555, "anton", "/start "+code))
	if got := h.api.sentTo(aliceBotToken, 555); len(got) != 2 || got[1] != telegramReplyAlreadyLinked {
		t.Fatalf("replies to a repeated /start: %q", got)
	}
	if link := h.link(aliceID); link.State != "linked" || link.ChatID != 555 {
		t.Fatalf("a repeated /start changed the link: %+v", link)
	}
}

func TestTelegramExpiredOrWrongCodeIsRefusedWithoutNamingTheAccount(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	code := h.issueCode(alice)["code"].(string)
	h.deliver(aliceBotToken, privateMessage(555, "anton", "/start wrong-"+code))
	// Код, который хранилище не примет и по форме (несколько слов, длиннее
	// 256 байт), получает тот же отказ, а не «повторите позже».
	h.deliver(aliceBotToken, privateMessage(555, "anton", "/start a b"))
	h.deliver(aliceBotToken, privateMessage(555, "anton", "/start "+strings.Repeat("x", 300)))
	h.clock.Advance(telegramLinkCodeTTL)
	h.deliver(aliceBotToken, privateMessage(555, "anton", "/start "+code))
	got := h.api.sentTo(aliceBotToken, 555)
	if !slices.Equal(got, slices.Repeat([]string{telegramReplyCodeRefused}, 4)) {
		t.Fatalf("replies to a wrong, a malformed and an expired code: %q", got)
	}
	for _, text := range got {
		if strings.Contains(text, "alice") || strings.Contains(text, aliceID) {
			t.Fatalf("refusal names the account: %q", text)
		}
	}
	if link := h.link(aliceID); link.State != "unlinked" || link.ChatID != 0 {
		t.Fatalf("a refused code linked the chat: %+v", link)
	}
}

func TestTelegramGroupsAndForeignChatsAreIgnored(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	code := h.issueCode(alice)["code"].(string)
	// Группа с действующим кодом: ни ответа, ни привязки.
	h.deliver(aliceBotToken, chatMessage(-100777, "group", 555, "anton", "/start "+code))
	h.deliver(aliceBotToken, chatMessage(-100778, "supergroup", 555, "anton", "/help"))
	if link := h.link(aliceID); link.State != "unlinked" || len(h.api.sent(aliceBotToken)) != 0 {
		t.Fatalf("a group update had an effect: %+v, sent %v", link, h.api.sent(aliceBotToken))
	}
	h.deliver(aliceBotToken, privateMessage(555, "anton", "/start "+code))
	if h.link(aliceID).State != "linked" {
		t.Fatal("the code did not link the private chat")
	}
	// Чужой личный чат привязанного бота: ни ответа, ни записи.
	for _, text := range []string{"/start " + code, "/start", "/help", "hello"} {
		h.deliver(aliceBotToken, privateMessage(777, "stranger", text))
	}
	if got := h.api.sentTo(aliceBotToken, 777); len(got) != 0 {
		t.Fatalf("a foreign chat got replies: %q", got)
	}
	if link := h.link(aliceID); link.ChatID != 555 || link.State != "linked" {
		t.Fatalf("a foreign chat changed the link: %+v", link)
	}
}

func TestTelegramLinkedChatGetsHelpForUnknownCommands(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.linkChat(alice, aliceBotToken, 555, "anton")
	h.deliver(aliceBotToken, privateMessage(555, "anton", "/status"))
	h.deliver(aliceBotToken, privateMessage(555, "anton", "/help@Alice_Bot"))
	got := h.api.sentTo(aliceBotToken, 555)
	if len(got) != 3 || got[1] != telegramReplyHelp || got[2] != telegramReplyHelp {
		t.Fatalf("replies to unknown commands: %q", got)
	}
}

func TestTelegramUnauthorizedTokenFailsTheLinkAndStopsThePoller(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.linkChat(alice, aliceBotToken, 555, "anton")
	h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		fakeRefuse(w, http.StatusUnauthorized, "Unauthorized for bot"+aliceBotToken)
		return true
	})
	telegramEventually(t, "the link fails", func() bool { return h.link(aliceID).State == "error" })
	reason := h.link(aliceID).Reason
	if !strings.Contains(reason, "401") || strings.Contains(reason, aliceBotSecret) {
		t.Fatalf("link error reason %q", reason)
	}
	telegramEventually(t, "the poller stops", func() bool { return !h.srv.telegram.running(aliceID) })
	if !h.quiet(aliceBotToken, 300*time.Millisecond) {
		t.Fatal("a rejected token is still polled")
	}
	_, data, raw := h.do(alice, "GET", "/api/me/telegram", "", nil)
	if data["state"] != "error" || data["reason"] != reason || strings.Contains(raw, aliceBotSecret) {
		t.Fatalf("view of the failed link: %s", raw)
	}
}

func TestTelegramConflictFailsTheLinkAndStopsThePoller(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		fakeRefuse(w, http.StatusConflict, "Conflict: terminated by other getUpdates request; make sure that only one bot instance is running")
		return true
	})
	telegramEventually(t, "the link fails", func() bool { return h.link(aliceID).State == "error" })
	if reason := h.link(aliceID).Reason; !strings.Contains(reason, "409") {
		t.Fatalf("link error reason %q", reason)
	}
	telegramEventually(t, "the poller stops", func() bool { return !h.srv.telegram.running(aliceID) })
	if !h.quiet(aliceBotToken, 300*time.Millisecond) {
		t.Fatal("a conflicting bot is still polled")
	}
	// Новый код снимает ошибку и снова запускает поллер.
	h.api.script(aliceBotToken, nil)
	polls := len(h.api.polls(aliceBotToken))
	h.issueCode(alice)
	telegramEventually(t, "the poller restarts", func() bool { return len(h.api.polls(aliceBotToken)) > polls })
}

func TestTelegramTooLargeBatchIsAskedAgainWithLimitOne(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	code := h.issueCode(alice)["code"].(string)
	huge := `{"ok":true,"result":[],"padding":"` + strings.Repeat("x", 8<<20) + `"}`
	answered := false
	h.api.script(aliceBotToken, func(w http.ResponseWriter, p fakePoll) bool {
		if answered || p.Limit != 100 {
			return false
		}
		answered = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(huge))
		return true
	})
	id := h.api.push(aliceBotToken, privateMessage(555, "anton", "/start "+code))
	telegramEventually(t, "the poller confirms the update", func() bool { return h.api.confirmed(aliceBotToken, id) })
	polls := h.api.polls(aliceBotToken)
	at := slices.IndexFunc(polls, func(p fakePoll) bool { return p.Limit == 1 })
	if at < 1 || polls[at].Offset != polls[at-1].Offset || polls[at-1].Limit != 100 {
		t.Fatalf("polls after the oversized batch: %+v", polls)
	}
	if last := polls[len(polls)-1]; last.Limit != 100 || last.Offset != id+1 {
		t.Fatalf("polling did not go on with full batches: %+v", polls)
	}
	if h.link(aliceID).State != "linked" {
		t.Fatal("the update fetched with Limit 1 was not handled")
	}
}

func TestTelegramDisabledUserStopsThePoller(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	if err := h.srv.store.SetUserDisabled(context.Background(), aliceID, true); err != nil {
		t.Fatal(err)
	}
	telegramEventually(t, "the poller stops", func() bool { return !h.srv.telegram.running(aliceID) })
	if !h.quiet(aliceBotToken, 300*time.Millisecond) {
		t.Fatal("a disabled user's bot is still polled")
	}
	// Включённый снова пользователь без привязанного чата получает поллер.
	if err := h.srv.store.SetUserDisabled(context.Background(), aliceID, false); err != nil {
		t.Fatal(err)
	}
	telegramEventually(t, "the poller restarts", func() bool { return h.srv.telegram.running(aliceID) })
}

func TestTelegramSessionRevocationStopsThePoller(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.linkChat(alice, aliceBotToken, 555, "anton")
	if _, err := h.srv.store.DeleteSessionsOfUser(context.Background(), aliceID); err != nil {
		t.Fatal(err)
	}
	telegramEventually(t, "the poller stops", func() bool { return !h.srv.telegram.running(aliceID) })
	if link := h.link(aliceID); link.State != "error" {
		t.Fatalf("revoked link: %+v", link)
	}
	if !h.quiet(aliceBotToken, 300*time.Millisecond) {
		t.Fatal("a revoked link's bot is still polled")
	}
}

func TestTelegramDeleteUnlinksAndStopsThePoller(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.linkChat(alice, aliceBotToken, 555, "anton")
	poller := h.poller(aliceID)
	resp, data := h.answersAfterSync(alice, "DELETE", "/api/me/telegram", nil)
	expectStatus(t, "DELETE", resp, data, http.StatusOK)
	if h.srv.telegram.running(aliceID) || poller == nil || poller.alive() {
		t.Fatal("DELETE returned before the poller stopped")
	}
	if data["own_bot"] != false || data["state"] != "unlinked" || data["chat"] != nil || data["bot_username"] != "" {
		t.Fatalf("view after DELETE: %v", data)
	}
	if !h.quiet(aliceBotToken, 300*time.Millisecond) {
		t.Fatal("an unlinked bot is still polled")
	}
}

func TestTelegramInstallationBotServesUsersWithoutAnOwnBot(t *testing.T) {
	h := newTelegramHarness(t, installBotToken, nil)
	bob, bobID := h.member("bob")
	telegramEventually(t, "the installation bot is polled", func() bool { return len(h.api.polls(installBotToken)) > 0 })
	telegramEventually(t, "the installation bot name is known", func() bool {
		_, data, _ := h.do(bob, "GET", "/api/me/telegram", "", nil)
		return data["bot_username"] == "brood_bot"
	})
	_, data, raw := h.do(bob, "GET", "/api/me/telegram", "", nil)
	if data["own_bot"] != false || strings.Contains(raw, installSecret) {
		t.Fatalf("installation view: %s", raw)
	}
	link, linkRaw := h.issueCodeRaw(bob)
	code := link["code"].(string)
	if link["url"] != "https://t.me/brood_bot?start="+code {
		t.Fatalf("installation link: %v", link)
	}
	if strings.Contains(linkRaw, installSecret) || strings.Contains(linkRaw, installBotToken) {
		t.Fatalf("installation link carries the bot token: %s", linkRaw)
	}
	h.deliver(installBotToken, privateMessage(666, "bobby", "/start "+code))
	if got := h.link(bobID); got.State != "linked" || got.ChatID != 666 {
		t.Fatalf("installation link: %+v", got)
	}
	h.deliver(installBotToken, privateMessage(666, "bobby", "/status"))
	if got := h.api.sentTo(installBotToken, 666); !slices.Equal(got, []string{telegramReplyLinked, telegramReplyHelp}) {
		t.Fatalf("installation replies: %q", got)
	}
	// Чат, не привязанный ни к кому, получает от бота установки только
	// ответ на код; прочее — без ответа.
	h.deliver(installBotToken, privateMessage(888, "stranger", "/status"))
	h.deliver(installBotToken, privateMessage(888, "stranger", "hello"))
	if got := h.api.sentTo(installBotToken, 888); len(got) != 0 {
		t.Fatalf("an unlinked chat got replies: %q", got)
	}
}

func TestTelegramCodeIsTakenOnlyByTheBotOfTheLink(t *testing.T) {
	h := newTelegramHarness(t, installBotToken, nil)
	alice, aliceID := h.member("alice")
	bob, bobID := h.member("bob")
	h.setBot(alice, aliceID, aliceBotToken)
	telegramEventually(t, "the installation bot is polled", func() bool { return len(h.api.polls(installBotToken)) > 0 })
	// Код bob идёт через бота установки; свой бот alice его не принимает.
	bobCode := h.issueCode(bob)["code"].(string)
	h.deliver(aliceBotToken, privateMessage(666, "bobby", "/start "+bobCode))
	if got := h.api.sentTo(aliceBotToken, 666); !slices.Equal(got, []string{telegramReplyCodeRefused}) {
		t.Fatalf("own bot replies to a code of the installation bot: %q", got)
	}
	aliceCode := h.issueCode(alice)["code"].(string)
	h.deliver(installBotToken, privateMessage(555, "anton", "/start "+aliceCode))
	if got := h.api.sentTo(installBotToken, 555); !slices.Equal(got, []string{telegramReplyCodeRefused}) {
		t.Fatalf("installation bot replies to a code of an own bot: %q", got)
	}
	if h.link(aliceID).State != "unlinked" || h.link(bobID).State != "unlinked" {
		t.Fatal("a code was taken by the wrong bot")
	}
}

func TestTelegramInstallationBotIsNotAnOwnBot(t *testing.T) {
	h := newTelegramHarness(t, installBotToken, nil)
	alice, aliceID := h.member("alice")
	resp, data, raw := h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": installBotToken})
	expectStatus(t, "PUT of the installation bot", resp, data, http.StatusConflict)
	expectTelegramRefusal(t, "PUT of the installation bot", data, "this is the installation bot: users without an own bot already use it")
	if strings.Contains(raw, installSecret) || h.link(aliceID).HasBotToken {
		t.Fatalf("installation bot as an own bot: %s", raw)
	}
}

// Пока getMe бота установки не ответил, его имени нет, и хранилище бота
// установки не узнаёт: сервер узнаёт его по id из токена.
func TestTelegramInstallationBotIsNotAnOwnBotBeforeItsNameIsKnown(t *testing.T) {
	api := newTestBotAPI(t)
	api.getMeDown(installBotToken, true)
	// Опрос бота установки висит до конца теста: поллер не спросит getMe
	// снова, когда getMe поднимется для проверки токена в PUT.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	api.script(installBotToken, func(http.ResponseWriter, fakePoll) bool {
		<-release
		return false
	})
	h := startTelegramHarness(t, api, "", installBotToken, nil)
	alice, aliceID := h.member("alice")
	telegramEventually(t, "the installation bot is polled", func() bool { return len(api.polls(installBotToken)) > 0 })
	if name, _ := h.srv.telegram.installation(); name != "" {
		t.Fatalf("installation bot name %q is known without getMe", name)
	}
	api.getMeDown(installBotToken, false)
	resp, data, raw := h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": installBotToken})
	expectStatus(t, "PUT of the installation bot", resp, data, http.StatusConflict)
	expectTelegramRefusal(t, "PUT of the installation bot", data, "this is the installation bot: users without an own bot already use it")
	if strings.Contains(raw, installSecret) || h.link(aliceID).HasBotToken {
		t.Fatalf("installation bot as an own bot: %s", raw)
	}
}

func TestTelegramInstallationBotStopsOnAnUnauthorizedToken(t *testing.T) {
	h := newTelegramHarness(t, installBotToken, nil)
	bob, _ := h.member("bob")
	h.api.script(installBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		fakeRefuse(w, http.StatusUnauthorized, "Unauthorized")
		return true
	})
	telegramEventually(t, "the installation poller stops", func() bool { return !h.srv.telegram.running("") })
	if !h.quiet(installBotToken, 300*time.Millisecond) {
		t.Fatal("a rejected installation bot is still polled")
	}
	_, data, raw := h.do(bob, "GET", "/api/me/telegram", "", nil)
	if reason, _ := data["bot_reason"].(string); !strings.Contains(reason, "401") || strings.Contains(raw, installSecret) {
		t.Fatalf("view of a failed installation bot: %s", raw)
	}
}

func TestTelegramNewTokenReplacesThePollerOfTheOldBot(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.setBot(alice, aliceID, bobBotToken)
	if !h.quiet(aliceBotToken, 300*time.Millisecond) {
		t.Fatal("the bot of the replaced token is still polled")
	}
	if !h.srv.telegram.running(aliceID) || h.link(aliceID).BotUsername != "bob_bot" {
		t.Fatalf("the new bot has no poller: %+v", h.link(aliceID))
	}
}

// 401 на старый токен приходит, когда новый уже записан: ошибка старого
// новому не достаётся, и поллер нового запускается.
func TestTelegramFatalExitOfAReplacedTokenSparesTheNewToken(t *testing.T) {
	// Сверка только по просьбе: о новом токене, записанном мимо API, сервер
	// узнаёт от остановленного поллера.
	h := startTelegramHarness(t, newTestBotAPI(t), "", "", nil, func(o *telegramOptions) { o.reconcile = time.Hour })
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	var replaced sync.Once
	h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		replaced.Do(func() {
			if _, err := h.srv.store.SetTelegramBot(context.Background(), aliceID, bobBotToken, "bob_bot", ""); err != nil {
				t.Error(err)
			}
		})
		fakeRefuse(w, http.StatusUnauthorized, "Unauthorized")
		return true
	})
	telegramEventually(t, "the bot of the new token is polled", func() bool { return len(h.api.polls(bobBotToken)) > 0 })
	if link := h.link(aliceID); link.State == "error" || link.BotUsername != "bob_bot" {
		t.Fatalf("the refusal of the old token failed the new one: %+v", link)
	}
	if !h.srv.telegram.running(aliceID) {
		t.Fatal("the poller of the new token is not running")
	}
}

func TestTelegramDeleteUnlinksAChatOfTheInstallationBot(t *testing.T) {
	h := newTelegramHarness(t, installBotToken, nil)
	bob, bobID := h.member("bob")
	telegramEventually(t, "the installation bot is polled", func() bool { return len(h.api.polls(installBotToken)) > 0 })
	h.linkChat(bob, installBotToken, 666, "bobby")
	if h.link(bobID).State != "linked" || h.telegramSessions(bobID) != 1 {
		t.Fatalf("installation link: %+v", h.link(bobID))
	}
	resp, data, _ := h.do(bob, "DELETE", "/api/me/telegram", h.csrf(bob), nil)
	expectStatus(t, "DELETE", resp, data, http.StatusOK)
	if data["state"] != "unlinked" || data["chat"] != nil {
		t.Fatalf("view after DELETE: %v", data)
	}
	if link := h.link(bobID); link.State != "unlinked" || link.ChatID != 0 || h.telegramSessions(bobID) != 0 {
		t.Fatalf("DELETE kept the installation link: %+v", link)
	}
	h.deliver(installBotToken, privateMessage(666, "bobby", "/status"))
	if got := h.api.sentTo(installBotToken, 666); !slices.Equal(got, []string{telegramReplyLinked}) {
		t.Fatalf("the unlinked chat still gets replies: %q", got)
	}
}

// Свой бот, ставший ботом установки: второго поллера одного бота нет, а
// привязка своего бота уходит в error с постоянной причиной.
func TestTelegramOwnBotThatIsTheInstallationBotIsNotPolledTwice(t *testing.T) {
	var aliceID string
	h := newTelegramHarness(t, installBotToken, func(ctx context.Context, st *store.Store) {
		aliceID = createTelegramUser(t, ctx, st, "alice")
		if _, err := st.SetTelegramBot(ctx, aliceID, installBotToken, "brood_bot", ""); err != nil {
			t.Fatal(err)
		}
	})
	telegramEventually(t, "the installation bot is polled", func() bool { return len(h.api.polls(installBotToken)) > 0 })
	telegramEventually(t, "the own link fails", func() bool { return h.link(aliceID).State == "error" })
	if reason := h.link(aliceID).Reason; reason != "this bot is now the installation bot: remove it as your own bot or set another bot token" {
		t.Fatalf("link error reason %q", reason)
	}
	if h.srv.telegram.running(aliceID) || !h.srv.telegram.running("") {
		t.Fatal("the installation bot is polled as an own bot too")
	}
	// Код через такой бот не выдаётся: сверка тут же снова сняла бы его.
	alice := h.signIn("alice")
	resp, data, _ := h.do(alice, "POST", "/api/me/telegram/link", h.csrf(alice), nil)
	expectStatus(t, "link through an own bot that is the installation bot", resp, data, http.StatusConflict)
	expectTelegramRefusal(t, "link through an own bot that is the installation bot", data, telegramReasonInstallationBot)
	if link := h.link(aliceID); link.State != "error" || link.LinkCodeExpiresAt != "" {
		t.Fatalf("a code was issued through an own bot that is the installation bot: %+v", link)
	}
}

// Ошибку привязки не удалось записать: поллер, остановленный 401, не
// перезапускается сверкой, пока не придёт новый код или токен.
func TestTelegramFatalExitStaysStoppedWhenItsErrorIsNotRecorded(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.sql(`CREATE TRIGGER telegram_link_error_fails BEFORE UPDATE OF state ON user_telegram_links WHEN NEW.state = 'error' BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	var refusals atomic.Int32
	h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		refusals.Add(1)
		fakeRefuse(w, http.StatusUnauthorized, "Unauthorized")
		return true
	})
	telegramEventually(t, "the poller stops", func() bool { return refusals.Load() > 0 && !h.srv.telegram.running(aliceID) })
	if !h.quiet(aliceBotToken, 300*time.Millisecond) || refusals.Load() != 1 {
		t.Fatalf("a poller whose error was not recorded restarts: %d refused polls", refusals.Load())
	}
	h.sql(`DROP TRIGGER telegram_link_error_fails`)
	h.api.script(aliceBotToken, nil)
	polls := len(h.api.polls(aliceBotToken))
	h.issueCode(alice)
	telegramEventually(t, "a new code restarts the poller", func() bool { return len(h.api.polls(aliceBotToken)) > polls })
}

// После отказа 429 следующий опрос ждёт не меньше retry_after, а не
// только backoff.
func TestTelegramRateLimitPausesAtLeastRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var failures []int
	h := startTelegramHarness(t, newTestBotAPI(t), "", "", nil, func(o *telegramOptions) {
		o.backoff = func(n int) time.Duration {
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, n)
			return 5 * time.Millisecond
		}
	})
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	var refusedAt, nextAt time.Time
	h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		mu.Lock()
		defer mu.Unlock()
		if refusedAt.IsZero() {
			refusedAt = time.Now()
			fakeRefuseRetry(w, http.StatusTooManyRequests, "Too Many Requests: retry after 1", 1)
			return true
		}
		if nextAt.IsZero() {
			nextAt = time.Now()
		}
		return false
	})
	telegramEventually(t, "polling goes on after the rate limit", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return !nextAt.IsZero()
	})
	mu.Lock()
	defer mu.Unlock()
	if gap := nextAt.Sub(refusedAt); gap < time.Second {
		t.Fatalf("the next poll came %s after a 429 with retry_after 1s", gap)
	}
	if !slices.Equal(failures, []int{1}) {
		t.Fatalf("backoff consulted for failures %v, want one first failure", failures)
	}
}

// Порция с id ниже offset (Telegram выбрал id заново): поллер её
// обрабатывает один раз и идёт дальше от её id, а не повторяет её.
func TestTelegramBatchBelowTheOffsetIsHandledOnceAndPollingGoesOn(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.linkChat(alice, aliceBotToken, 555, "anton")
	const low = 5
	update := privateMessage(555, "anton", "/status")
	update["update_id"] = int64(low)
	h.api.script(aliceBotToken, func(w http.ResponseWriter, p fakePoll) bool {
		if p.Offset == low+1 {
			return false
		}
		// Bot API отвечает с начала очереди, пока offset выше всех её id.
		fakeResult(w, []map[string]any{update})
		return true
	})
	telegramEventually(t, "polling goes on after the renumbered update", func() bool {
		return slices.ContainsFunc(h.api.polls(aliceBotToken), func(p fakePoll) bool { return p.Offset == low+1 })
	})
	time.Sleep(100 * time.Millisecond)
	if got := h.api.sentTo(aliceBotToken, 555); !slices.Equal(got, []string{telegramReplyLinked, telegramReplyHelp}) {
		t.Fatalf("replies to the renumbered update: %q", got)
	}
}

func TestTelegramCodeFromAChatOfAnotherAccountIsRefused(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	bob, bobID := h.member("bob")
	h.setBot(bob, bobID, bobBotToken)
	h.linkChat(bob, bobBotToken, 555, "anton")
	h.setBot(alice, aliceID, aliceBotToken)
	code := h.issueCode(alice)["code"].(string)
	h.deliver(aliceBotToken, privateMessage(555, "anton", "/start "+code))
	if got := h.api.sentTo(aliceBotToken, 555); !slices.Equal(got, []string{telegramReplyChatTaken}) {
		t.Fatalf("replies to a code from a chat of another account: %q", got)
	}
	if h.link(aliceID).State != "unlinked" || h.link(bobID).ChatID != 555 {
		t.Fatalf("a taken chat changed a link: alice %+v, bob %+v", h.link(aliceID), h.link(bobID))
	}
}

// Отказ клиента несёт текст, в котором хранилище видит токен (здесь — чужой
// токен с закодированным двоеточием): причина остаётся постоянной частью.
func TestTelegramReasonTheStoreRefusesKeepsTheConstantPart(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		fakeRefuse(w, http.StatusUnauthorized, "Unauthorized: token 710000123%3ASENTINEL-foreign_bot-0123456789abcdefgh was revoked")
		return true
	})
	telegramEventually(t, "the link fails", func() bool { return h.link(aliceID).State == "error" })
	if reason := h.link(aliceID).Reason; reason != telegramReasonUnauthorized {
		t.Fatalf("link error reason %q, want the constant part", reason)
	}
}

// Личный чат слышен только от самого его человека: отправитель, чей id не
// равен id чата, и бот-отправитель игнорируются без ответа.
func TestTelegramOnlyThePersonOfThePrivateChatIsHeard(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	code := h.issueCode(alice)["code"].(string)
	h.deliver(aliceBotToken, chatMessage(555, "private", 556, "anton", "/start "+code))
	fromBot := privateMessage(557, "robot_bot", "/start "+code)
	fromBot["message"].(map[string]any)["from"].(map[string]any)["is_bot"] = true
	h.deliver(aliceBotToken, fromBot)
	if sent := h.api.sent(aliceBotToken); len(sent) != 0 {
		t.Fatalf("replies to a sender other than the chat's person: %+v", sent)
	}
	if link := h.link(aliceID); link.State != "unlinked" {
		t.Fatalf("a sender other than the chat's person linked it: %+v", link)
	}
}

// Обновление, чью обработку прервала остановка поллера, не подтверждается:
// следующий поллер этого бота спрашивает с него.
func TestTelegramInterruptedUpdateIsNotConfirmed(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.linkChat(alice, aliceBotToken, 555, "anton")
	replying := make(chan struct{})
	var once sync.Once
	h.api.scriptSend(aliceBotToken, func(_ http.ResponseWriter, r *http.Request, s fakeSent) bool {
		if s.Text != telegramReplyHelp {
			return false
		}
		once.Do(func() { close(replying) })
		<-r.Context().Done()
		return true
	})
	id := h.api.push(aliceBotToken, privateMessage(555, "anton", "/status"))
	select {
	case <-replying:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting: the poller replies to the update")
	}
	resp, data, _ := h.do(alice, "DELETE", "/api/me/telegram", h.csrf(alice), nil)
	expectStatus(t, "DELETE", resp, data, http.StatusOK)
	h.api.scriptSend(aliceBotToken, nil)
	polls := len(h.api.polls(aliceBotToken))
	resp, data, _ = h.do(alice, "PUT", "/api/me/telegram", h.csrf(alice), map[string]any{"token": aliceBotToken})
	expectStatus(t, "PUT", resp, data, http.StatusOK)
	telegramEventually(t, "the new poller polls", func() bool { return len(h.api.polls(aliceBotToken)) > polls })
	if next := h.api.polls(aliceBotToken)[polls]; next.Offset != id {
		t.Fatalf("the next poller asks from offset %d, want the interrupted update %d", next.Offset, id)
	}
}

// Новый код запускает поллер, остановленный ошибкой, до ответа: код сразу
// можно отправить боту.
func TestTelegramLinkCodeRestartsAFailedPollerBeforeTheAnswer(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		fakeRefuse(w, http.StatusConflict, "Conflict: terminated by other getUpdates request")
		return true
	})
	telegramEventually(t, "the poller stops", func() bool {
		return h.link(aliceID).State == "error" && !h.srv.telegram.running(aliceID)
	})
	h.api.script(aliceBotToken, nil)
	resp, data := h.answersAfterSync(alice, "POST", "/api/me/telegram/link", nil)
	expectStatus(t, "POST /api/me/telegram/link", resp, data, http.StatusCreated)
	if !h.srv.telegram.running(aliceID) {
		t.Fatal("the link code was answered before the poller restarted")
	}
}

// Имя бота установки из getMe, не годное для ссылки t.me, считается
// неизвестным: ни профиль, ни ссылка его не несут.
func TestTelegramInstallationBotNameMustBeAUsername(t *testing.T) {
	api := newFakeBotAPI(t)
	api.addBot(installBotToken, "brood_bot?start=forged#")
	h := startTelegramHarness(t, api, "", installBotToken, nil)
	bob, _ := h.member("bob")
	telegramEventually(t, "the installation bot is polled", func() bool { return len(api.polls(installBotToken)) > 0 })
	if name, _ := h.srv.telegram.installation(); name != "" {
		t.Fatalf("installation bot name %q taken from getMe", name)
	}
	_, data, _ := h.do(bob, "GET", "/api/me/telegram", "", nil)
	if data["bot_username"] != "" {
		t.Fatalf("profile names the installation bot %v", data["bot_username"])
	}
	link := h.issueCode(bob)
	if _, ok := link["url"]; ok || link["bot_username"] != "" {
		t.Fatalf("link through an unnamed installation bot: %v", link)
	}
}

// Привязку чата, которую поллер знает в памяти, перепроверяет хранилище:
// отзыв сессий мимо API (блокировка, смена пароля или роли) лишает чат
// привязки ещё до следующей сверки, и команда остаётся без ответа.
func TestTelegramRevokedChatIsNotHeardBeforeTheNextReconcile(t *testing.T) {
	for _, tc := range []struct {
		name, install, token, bot string
	}{
		{"installation bot", installBotToken, installBotToken, ""},
		{"own bot", "", bobBotToken, "bob_bot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Сверка только по просьбе, а код подтверждает хранилище, мимо
			// поллера и его просьбы о сверке: набор в памяти до конца теста
			// не меняется.
			h := startTelegramHarness(t, newTestBotAPI(t), "", tc.install, nil, func(o *telegramOptions) { o.reconcile = time.Hour })
			bob, bobID := h.member("bob")
			if tc.install == "" {
				h.setBot(bob, bobID, bobBotToken)
			}
			telegramEventually(t, "the bot of the link is polled", func() bool { return len(h.api.polls(tc.token)) > 0 })
			h.confirmInStore(tc.bot, h.issueCode(bob)["code"].(string), 666)
			if err := h.srv.telegram.sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			h.deliver(tc.token, privateMessage(666, "bobby", "/status"))
			if got := h.api.sentTo(tc.token, 666); !slices.Equal(got, []string{telegramReplyHelp}) {
				t.Fatalf("replies to the linked chat: %q", got)
			}
			if _, err := h.srv.store.DeleteSessionsOfUser(context.Background(), bobID); err != nil {
				t.Fatal(err)
			}
			if tc.install != "" && h.srv.telegram.installationChat(666) != bobID {
				t.Fatal("the pollers forgot the chat before the next reconcile")
			}
			h.deliver(tc.token, privateMessage(666, "bobby", "/status"))
			if got := h.api.sentTo(tc.token, 666); !slices.Equal(got, []string{telegramReplyHelp}) {
				t.Fatalf("a revoked chat still gets replies: %q", got)
			}
		})
	}
}

// Тот же токен, сохранённый снова после 409 (второй поллер остановлен),
// снимает пометку остановки: поллер запускается до ответа PUT.
func TestTelegramSavingTheSameTokenRestartsAHaltedPoller(t *testing.T) {
	h := newTelegramHarness(t, "", nil)
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		fakeRefuse(w, http.StatusConflict, "Conflict: terminated by other getUpdates request")
		return true
	})
	telegramEventually(t, "the poller stops", func() bool {
		return h.link(aliceID).State == "error" && !h.srv.telegram.running(aliceID)
	})
	h.api.script(aliceBotToken, nil)
	polls := len(h.api.polls(aliceBotToken))
	resp, data := h.answersAfterSync(alice, "PUT", "/api/me/telegram", map[string]any{"token": aliceBotToken})
	expectStatus(t, "PUT of the same token", resp, data, http.StatusOK)
	if data["state"] != "unlinked" || !h.srv.telegram.running(aliceID) {
		t.Fatalf("PUT of the same token answered before the poller restarted: %v", data)
	}
	telegramEventually(t, "getUpdates resumes", func() bool { return len(h.api.polls(aliceBotToken)) > polls })
}

// Server.Close ждёт поллеры Telegram до закрытия базы: поллер, который
// пишет ошибку привязки после отмены (failLink её переживает), дописывает её
// в открытую базу, и Close возвращается только после него.
func TestTelegramServerCloseWaitsForThePollersBeforeTheStore(t *testing.T) {
	h := startTelegramHarness(t, newTestBotAPI(t), "", "", nil, func(o *telegramOptions) { o.reconcile = time.Hour })
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	poller := h.poller(aliceID)
	// Чужая блокировка записи держит запись ошибки привязки в ожидании.
	release := h.lockWrites()
	h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
		fakeRefuse(w, http.StatusUnauthorized, "Unauthorized")
		return true
	})
	telegramEventually(t, "the poller writes its link error", func() bool { return h.halted(aliceID) })
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		h.srv.Close()
	}()
	select {
	case <-closed:
		release()
		t.Fatal("Close returned while a poller was writing its link error")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting: Close returns")
	}
	if poller.alive() {
		t.Fatal("Close returned before the poller stopped")
	}
	if state := h.column(`SELECT state FROM user_telegram_links WHERE user_id = ?`, aliceID); state != "error" {
		t.Fatalf("the link error of the stopping poller did not reach the database: state %q", state)
	}
}

// База не прочлась при сверке после DELETE: поллер снятого бота всё равно
// остановлен до ответа.
func TestTelegramDeleteStopsThePollerWhenTheReconcileFails(t *testing.T) {
	h := startTelegramHarness(t, newTestBotAPI(t), "", "", nil, func(o *telegramOptions) { o.reconcile = time.Hour })
	alice, aliceID := h.member("alice")
	h.setBot(alice, aliceID, aliceBotToken)
	poller := h.poller(aliceID)
	h.breakReconcile()
	resp, data, _ := h.do(alice, "DELETE", "/api/me/telegram", h.csrf(alice), nil)
	expectStatus(t, "DELETE", resp, data, http.StatusOK)
	if poller.alive() || h.srv.telegram.running(aliceID) {
		t.Fatal("DELETE answered while the poller of the removed bot runs")
	}
	if !h.quiet(aliceBotToken, 300*time.Millisecond) {
		t.Fatal("the removed bot is still polled")
	}
}

// Запись PUT и POST /link обещает поллер до ответа. Сверка не прошла (база
// не прочлась): ответ — внутренний сбой, а не успех без поллера.
func TestTelegramWriteThatCannotReconcileThePollersIsAnInternalError(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		body         any
		prepare      func(t *testing.T, h *telegramHarness, client *http.Client, userID string)
	}{
		{"PUT", "/api/me/telegram", map[string]any{"token": aliceBotToken}, nil},
		{"POST", "/api/me/telegram/link", nil, func(t *testing.T, h *telegramHarness, client *http.Client, userID string) {
			h.setBot(client, userID, aliceBotToken)
			h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
				fakeRefuse(w, http.StatusConflict, "Conflict: terminated by other getUpdates request")
				return true
			})
			telegramEventually(t, "the poller stops", func() bool {
				return h.link(userID).State == "error" && !h.srv.telegram.running(userID)
			})
			h.api.script(aliceBotToken, nil)
		}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			h := startTelegramHarness(t, newTestBotAPI(t), "", "", nil, func(o *telegramOptions) { o.reconcile = time.Hour })
			alice, aliceID := h.member("alice")
			if tc.prepare != nil {
				tc.prepare(t, h, alice, aliceID)
			}
			h.breakReconcile()
			resp, data, raw := h.do(alice, tc.method, tc.path, h.csrf(alice), tc.body)
			expectStatus(t, tc.method+" without a reconcile", resp, data, http.StatusInternalServerError)
			if data["detail"] != "internal error" || strings.Contains(raw, aliceBotSecret) {
				t.Fatalf("%s without a reconcile: %s", tc.method, raw)
			}
			if h.srv.telegram.running(aliceID) {
				t.Fatalf("%s without a reconcile started a poller", tc.method)
			}
		})
	}
}

// Клиент ушёл сразу после фиксации записи: сверка набора поллеров не
// отменяется вместе с его запросом. Сверка только по просьбе, поэтому набор
// меняет лишь сам обработчик записи.
func TestTelegramWriteReconcilesThePollersAfterTheClientLeft(t *testing.T) {
	setBot := func(t *testing.T, h *telegramHarness, client *http.Client, userID string) {
		h.setBot(client, userID, aliceBotToken)
	}
	running := func(h *telegramHarness, userID string) bool { return h.srv.telegram.running(userID) }
	for _, tc := range []struct {
		method, path string
		body         any
		prepare      func(t *testing.T, h *telegramHarness, client *http.Client, userID string)
		reconciled   func(h *telegramHarness, userID string) bool
	}{
		{"PUT", "/api/me/telegram", map[string]any{"token": aliceBotToken}, nil, running},
		{"DELETE", "/api/me/telegram", nil, setBot, func(h *telegramHarness, userID string) bool { return !running(h, userID) }},
		{"POST", "/api/me/telegram/link", nil, func(t *testing.T, h *telegramHarness, client *http.Client, userID string) {
			setBot(t, h, client, userID)
			h.api.script(aliceBotToken, func(w http.ResponseWriter, _ fakePoll) bool {
				fakeRefuse(w, http.StatusConflict, "Conflict: terminated by other getUpdates request")
				return true
			})
			telegramEventually(t, "the poller stops", func() bool {
				return h.link(userID).State == "error" && !h.srv.telegram.running(userID)
			})
			h.api.script(aliceBotToken, nil)
		}, running},
	} {
		t.Run(tc.method, func(t *testing.T) {
			var leave atomic.Pointer[context.CancelFunc]
			h := startTelegramHarness(t, newTestBotAPI(t), "", "", nil, func(o *telegramOptions) {
				o.reconcile = time.Hour
				o.afterWrite = func(r *http.Request) {
					if r.Method != tc.method {
						return
					}
					cancel := leave.Swap(nil)
					if cancel == nil {
						return
					}
					(*cancel)()
					select {
					case <-r.Context().Done():
					case <-time.After(5 * time.Second):
						t.Error("the server did not see the client leave")
					}
				}
			})
			alice, aliceID := h.member("alice")
			if tc.prepare != nil {
				tc.prepare(t, h, alice, aliceID)
			}
			csrf := h.csrf(alice)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			leave.Store(&cancel)
			if _, _, err := telegramRequest(ctx, h.http.URL, alice, tc.method, tc.path, csrf, tc.body); err == nil {
				t.Fatalf("%s was answered after the client left", tc.method)
			}
			telegramEventually(t, "the write reconciles the pollers", func() bool { return tc.reconciled(h, aliceID) })
		})
	}
}

// getMe бота установки, ответивший именем, которое не годится, не
// спрашивается перед каждым getUpdates: ответ получен, имени просто нет.
func TestTelegramInstallationBotAsksGetMeOnceWhenItAnswers(t *testing.T) {
	api := newFakeBotAPI(t)
	api.addBot(installBotToken, "brood_bot?start=forged#")
	h := startTelegramHarness(t, api, "", installBotToken, nil)
	telegramEventually(t, "the installation bot is polled", func() bool { return len(api.polls(installBotToken)) > 0 })
	for range 3 {
		h.deliver(installBotToken, privateMessage(888, "stranger", "hello"))
	}
	if n := api.meCalls(installBotToken); n != 1 {
		t.Fatalf("getMe asked %d times over %d polls, want once", n, len(api.polls(installBotToken)))
	}
}

// Сбой getMe бота установки опрос не держит, а сам getMe спрашивается снова
// только после паузы backoff, как getUpdates после сбоя.
func TestTelegramInstallationBotAsksAFailedGetMeAgainOnlyAfterTheBackoff(t *testing.T) {
	api := newTestBotAPI(t)
	api.getMeDown(installBotToken, true)
	var mu sync.Mutex
	var failures []int
	h := startTelegramHarness(t, api, "", installBotToken, nil, func(o *telegramOptions) {
		o.backoff = func(n int) time.Duration {
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, n)
			return time.Hour
		}
	})
	telegramEventually(t, "the installation bot is polled", func() bool { return len(api.polls(installBotToken)) > 0 })
	for range 3 {
		h.deliver(installBotToken, privateMessage(888, "stranger", "hello"))
	}
	if n := api.meCalls(installBotToken); n != 1 {
		t.Fatalf("a failed getMe was asked %d times before its backoff passed", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(failures, []int{1}) {
		t.Fatalf("backoff consulted for failures %v, want one first failure", failures)
	}
}

// После паузы getMe спрашивается снова, и имя бота установки становится
// известно.
func TestTelegramInstallationBotLearnsItsNameAfterGetMeRecovers(t *testing.T) {
	api := newTestBotAPI(t)
	api.getMeDown(installBotToken, true)
	h := startTelegramHarness(t, api, "", installBotToken, nil)
	telegramEventually(t, "getMe of the installation bot fails", func() bool { return api.meCalls(installBotToken) > 0 })
	api.getMeDown(installBotToken, false)
	telegramEventually(t, "the installation bot name is known", func() bool {
		h.deliver(installBotToken, privateMessage(888, "stranger", "hello"))
		name, _ := h.srv.telegram.installation()
		return name == "brood_bot"
	})
}
