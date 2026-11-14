package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// FEATURE: Telegram link of a user (#279, epic #278).
// SPEC: one link per user. The bot token is write-only: the store view says
// only whether a token is set and names the bot; the token is read back only
// for the server's own Bot API client, and the installation token from .env is
// a fallback that the user's token overrides. A one-time link code of at
// least 16 bytes with a TTL of at most an hour, stored hashed and naming one
// user, binds one private chat (chat id equal to the sender id) through the
// bot the link uses; the TTL is checked against a moment the caller must
// name. A bot, a chat and a Telegram user belong to at most one brood user,
// and the installation bot to none; bot names compare ignoring case, as in
// Telegram. Dropping a link clears its waiting code. Linking opens a server
// session of kind telegram that the link references; an answer from Telegram
// requires that live session, owned by the user, for an update of the link's
// bot, in the answer transaction. Unlinking deletes the session, and revoking
// the user's sessions (disable, role change, password change) drops the link
// with a reason, as does deleting whatever session, of any kind, a link
// references. A link error reason never carries a bot token: known, shaped
// like one or in a Bot API URL, also under up to three layers of
// percent-encoding. A refusal writes nothing, not even a default link row,
// inside a caller's transaction. Tokens, codes, bot names and reasons have
// limits.
// IMPLEMENTATION: user_telegram_links, sessions.kind and the trigger
// sessions_telegram_revoke (telegram_schema.go), telegram_links.go.
// STATUS: DONE

// telegramTestTime — момент, от которого тесты считают срок кода привязки.
var telegramTestTime = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// telegramStore: пользователи u1 и u2 без привязок.
func telegramStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	seedUser(t, s, "u1", "alice")
	seedUser(t, s, "u2", "bob")
	return s
}

// linkCode: код привязки с именем name, длиной не меньше наименьшей длины
// кода.
func linkCode(name string) string { return "telegram-link-code-" + name }

// linkTelegram выдаёт пользователю код и подтверждает его из чата chat
// Telegram-пользователя chat без имени через бота bot ("" — бот установки).
func linkTelegram(t *testing.T, s *Store, user, bot string, chat int64) TelegramLink {
	t.Helper()
	return linkTelegramAs(t, s, user, bot, chat, "")
}

// linkTelegramAs — linkTelegram, у собеседника которого имя username ("" —
// без имени).
func linkTelegramAs(t *testing.T, s *Store, user, bot string, chat int64, username string) TelegramLink {
	t.Helper()
	ctx := context.Background()
	code := linkCode(user)
	if _, err := s.IssueTelegramLinkCode(ctx, user, code, telegramTestTime, telegramTestTime.Add(10*time.Minute)); err != nil {
		t.Fatalf("issue link code for %s: %v", user, err)
	}
	link, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{BotUsername: bot, Code: code, ChatID: chat, TelegramUserID: chat, Username: username, Now: telegramTestTime})
	if err != nil {
		t.Fatalf("confirm link of %s: %v", user, err)
	}
	return link
}

// telegramSessions: сессии вида telegram как «user_id:expires_at».
func telegramSessions(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT user_id || ':' || coalesce(expires_at, 'never') FROM sessions WHERE kind = 'telegram' ORDER BY user_id`)
	mustNil(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		mustNil(t, rows.Scan(&row))
		out = append(out, row)
	}
	mustNil(t, rows.Err())
	return out
}

// active: живо ли право пользователя user для обновления из чата chat от
// from, принятого ботом bot ("" — бот установки).
func active(t *testing.T, s *Store, user, bot string, chat, from int64) bool {
	t.Helper()
	ok, err := s.TelegramSessionActive(context.Background(), user, bot, chat, from)
	mustNil(t, err)
	return ok
}

func TestTelegramBotTokenIsWriteOnly(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	const token = "123456:SENTINEL-BOT-TOKEN"
	set, err := s.SetTelegramBot(ctx, "u1", token, "alice_brood_bot", "")
	mustNil(t, err)
	got, err := s.GetTelegramLink(ctx, "u1")
	mustNil(t, err)
	for name, link := range map[string]TelegramLink{"set": set, "get": got} {
		if !link.HasBotToken || link.BotUsername != "alice_brood_bot" || link.State != "unlinked" {
			t.Fatalf("%s view = %+v, want a token flag, the bot name and unlinked", name, link)
		}
		encoded, err := json.Marshal(link)
		mustNil(t, err)
		for _, text := range []string{fmt.Sprintf("%+v", link), fmt.Sprintf("%#v", link), string(encoded)} {
			if strings.Contains(text, "SENTINEL") {
				t.Fatalf("%s view exposes the bot token: %s", name, text)
			}
		}
	}
	// В представлении нет поля, куда токен мог бы попасть.
	view := reflect.TypeOf(TelegramLink{})
	for i := range view.NumField() {
		if strings.Contains(strings.ToLower(view.Field(i).Name), "token") && view.Field(i).Type.Kind() != reflect.Bool {
			t.Fatalf("TelegramLink carries %s: the bot token is write-only", view.Field(i).Name)
		}
	}
	// Единственный путь чтения — клиент Bot API сервера.
	if own, err := s.TelegramBotToken(ctx, "u1", ""); err != nil || own != token {
		t.Fatalf("server read of the token = %q, %v", own, err)
	}
	// Отказ на занятом боте не повторяет токен.
	if _, err := s.SetTelegramBot(ctx, "u2", "654321:SENTINEL-OTHER", "alice_brood_bot", ""); !errors.Is(err, ErrTelegramBotTaken) || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("taken bot error = %v, want ErrTelegramBotTaken without the token", err)
	}
	// Код привязки хранится хэшем: в строках базы его нет.
	_, err = s.IssueTelegramLinkCode(ctx, "u1", "SENTINEL-LINK-CODE", telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	for table, rows := range databaseRows(t, s.db) {
		for _, row := range rows {
			if strings.Contains(row, "SENTINEL-LINK-CODE") {
				t.Fatalf("table %s stores the link code in plain text: %s", table, row)
			}
		}
	}
}

func TestTelegramInstallationTokenIsAFallback(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	if token, err := s.TelegramBotToken(ctx, "u1", "installation-token"); err != nil || token != "installation-token" {
		t.Fatalf("token without an own bot = %q, %v; want the installation token", token, err)
	}
	if _, err := s.TelegramBotToken(ctx, "u1", ""); !isNotFound(err) {
		t.Fatalf("no own and no installation token = %v, want ErrNotFound", err)
	}
	if _, err := s.TelegramBotToken(ctx, "ghost", "installation-token"); !isNotFound(err) {
		t.Fatalf("unknown user = %v, want ErrNotFound", err)
	}
	_, err := s.SetTelegramBot(ctx, "u1", "111:own", "alice_brood_bot", "")
	mustNil(t, err)
	if token, err := s.TelegramBotToken(ctx, "u1", "installation-token"); err != nil || token != "111:own" {
		t.Fatalf("own token priority = %q, %v", token, err)
	}
	cleared, err := s.ClearTelegramBot(ctx, "u1")
	mustNil(t, err)
	if cleared.HasBotToken || cleared.BotUsername != "" {
		t.Fatalf("cleared view = %+v", cleared)
	}
	if token, err := s.TelegramBotToken(ctx, "u1", "installation-token"); err != nil || token != "installation-token" {
		t.Fatalf("token after clearing the own bot = %q, %v", token, err)
	}
	for _, in := range [][2]string{{"", "bot"}, {"tok", ""}, {"to ken", "bot"}, {"tok", "@bot"}} {
		if _, err := s.SetTelegramBot(ctx, "u1", in[0], in[1], ""); !errors.Is(err, ErrTelegramInvalid) {
			t.Fatalf("SetTelegramBot(%q, %q) = %v, want ErrTelegramInvalid", in[0], in[1], err)
		}
	}
	if _, err := s.SetTelegramBot(ctx, "ghost", "222:x", "ghost_bot", ""); !isNotFound(err) {
		t.Fatalf("bot of an unknown user = %v, want ErrNotFound", err)
	}
}

func TestTelegramLinkIsUniquePerUserBotAndChat(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	seedUser(t, s, "u3", "carol")
	_, err := s.SetTelegramBot(ctx, "u1", "111:a", "alice_brood_bot", "")
	mustNil(t, err)
	_, err = s.SetTelegramBot(ctx, "u1", "111:b", "alice_brood_bot", "")
	mustNil(t, err)
	var rows int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM user_telegram_links WHERE user_id = 'u1'`).Scan(&rows))
	if rows != 1 {
		t.Fatalf("links of u1 = %d, want one row per user", rows)
	}
	// Бот принадлежит одному пользователю: два поллера одного бота делили бы getUpdates.
	if _, err := s.SetTelegramBot(ctx, "u2", "111:c", "alice_brood_bot", ""); !errors.Is(err, ErrTelegramBotTaken) {
		t.Fatalf("second owner of a bot = %v, want ErrTelegramBotTaken", err)
	}
	if link, err := s.GetTelegramLink(ctx, "u2"); err != nil || link.HasBotToken {
		t.Fatalf("refused owner keeps a token: %+v, %v", link, err)
	}
	// Чат и пользователь Telegram принадлежат одному пользователю brood:
	// обновление из чата бота установки адресуется однозначно. Чат личный:
	// его id — id собеседника, поэтому занятый чат занимает и собеседника.
	linkTelegram(t, s, "u1", "alice_brood_bot", 42)
	_, err = s.IssueTelegramLinkCode(ctx, "u2", linkCode("u2"), telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	if _, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{Code: linkCode("u2"), ChatID: 42, TelegramUserID: 42, Now: telegramTestTime}); !errors.Is(err, ErrTelegramChatTaken) {
		t.Fatalf("chat linked twice = %v, want ErrTelegramChatTaken", err)
	}
	for name, in := range map[string]TelegramLinkConfirmation{
		"telegram user in another chat": {Code: linkCode("u2"), ChatID: 77, TelegramUserID: 42, Now: telegramTestTime},
		"another sender in the chat":    {Code: linkCode("u2"), ChatID: 42, TelegramUserID: 77, Now: telegramTestTime},
	} {
		if _, err := s.ConfirmTelegramLink(ctx, in); !errors.Is(err, ErrTelegramInvalid) {
			t.Fatalf("%s = %v, want ErrTelegramInvalid: only a private chat links", name, err)
		}
	}
	// Отказ откатывает подтверждение целиком: код цел, сессии нет.
	if got := telegramSessions(t, s); !reflect.DeepEqual(got, []string{"u1:never"}) {
		t.Fatalf("telegram sessions after refused links = %v", got)
	}
	linked, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{Code: linkCode("u2"), ChatID: 77, TelegramUserID: 77, Now: telegramTestTime})
	if err != nil || linked.State != "linked" || linked.ChatID != 77 {
		t.Fatalf("link of another chat = %+v, %v", linked, err)
	}
	// Один код называет одного пользователя: пока код действует, второй
	// пользователь его не получает.
	_, err = s.IssueTelegramLinkCode(ctx, "u3", linkCode("shared-code"), telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	seedUser(t, s, "u4", "dave")
	if _, err := s.IssueTelegramLinkCode(ctx, "u4", linkCode("shared-code"), telegramTestTime, telegramTestTime.Add(time.Minute)); !errors.Is(err, ErrTelegramInvalid) || !strings.Contains(err.Error(), "already issued") {
		t.Fatalf("one code for two users = %v, want the already-issued refusal", err)
	}
	// Схема — вторая линия: прямая запись тоже не дублирует привязку.
	for name, statement := range map[string]string{
		"second row of a user": `INSERT INTO user_telegram_links (user_id, state, events, updated_at) VALUES ('u1', 'unlinked', '[]', 'now')`,
		"taken chat":           `UPDATE user_telegram_links SET chat_id = 42, telegram_user_id = 42 WHERE user_id = 'u2'`,
		"taken bot":            `INSERT INTO user_telegram_links (user_id, bot_token, bot_username, state, events, updated_at) VALUES ('u4', '9:z', 'alice_brood_bot', 'unlinked', '[]', 'now')`,
		"shared session":       `UPDATE user_telegram_links SET session_hash = (SELECT session_hash FROM user_telegram_links WHERE user_id = 'u1') WHERE user_id = 'u2'`,
		"shared link code":     `INSERT INTO user_telegram_links (user_id, link_code_hash, link_code_expires_at, state, events, updated_at) SELECT 'u4', link_code_hash, link_code_expires_at, 'unlinked', '[]', 'now' FROM user_telegram_links WHERE user_id = 'u3'`,
	} {
		if _, err := s.db.Exec(statement); !IsUniqueViolation(err) {
			t.Fatalf("%s: %v, want a unique violation", name, err)
		}
	}
}

// TestInstallationBotIsNoUsersOwnBot: бот установки не становится своим ботом
// пользователя: поллер установки и поллер своего бота делили бы одну очередь
// getUpdates. Имя бота в Telegram не различает регистр.
func TestInstallationBotIsNoUsersOwnBot(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	before := databaseRows(t, s.db)
	for _, name := range []string{"brood_bot", "Brood_Bot"} {
		if _, err := s.SetTelegramBot(ctx, "u1", "555:installation", name, "brood_bot"); !errors.Is(err, ErrTelegramBotTaken) || !strings.Contains(err.Error(), "installation bot") {
			t.Fatalf("installation bot %q as an own bot = %v, want ErrTelegramBotTaken naming the installation bot", name, err)
		}
	}
	if after := databaseRows(t, s.db); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused installation bot changed the database:\n got %v\nwant %v", after, before)
	}
	if set, err := s.SetTelegramBot(ctx, "u1", "111:a", "alice_brood_bot", "brood_bot"); err != nil || set.BotUsername != "alice_brood_bot" {
		t.Fatalf("own bot next to the installation bot = %+v, %v", set, err)
	}
}

// TestTelegramBotNameIgnoresCaseEverywhere: Telegram не различает регистр
// имени бота, и хранилище тоже, не только для бота установки: тот же токен
// под именем в другом регистре — тот же занятый бот, код приходит и
// обновление действует через бота, названного в любом регистре, а новый
// токен того же бота привязку не снимает.
func TestTelegramBotNameIgnoresCaseEverywhere(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	const token = "123456:SENTINEL-SHARED-TOKEN"
	_, err := s.SetTelegramBot(ctx, "u1", token, "alice_brood_bot", "")
	mustNil(t, err)
	for _, name := range []string{"Alice_Brood_Bot", "ALICE_BROOD_BOT"} {
		if _, err := s.SetTelegramBot(ctx, "u2", token, name, ""); !errors.Is(err, ErrTelegramBotTaken) || strings.Contains(err.Error(), "SENTINEL") {
			t.Fatalf("the same token as %q for a second user = %v, want ErrTelegramBotTaken without the token", name, err)
		}
	}
	if link, err := s.GetTelegramLink(ctx, "u2"); err != nil || link.HasBotToken {
		t.Fatalf("refused owner keeps a token: %+v, %v", link, err)
	}
	// Схема — вторая линия: прямая запись тоже не делит бота.
	if _, err := s.db.Exec(`INSERT INTO user_telegram_links (user_id, bot_token, bot_username, state, events, updated_at) VALUES ('u2', '123456:SENTINEL-SHARED-TOKEN', 'Alice_Brood_Bot', 'unlinked', '[]', 'now')`); !IsUniqueViolation(err) {
		t.Fatalf("case variant of a taken bot written directly: %v, want a unique violation", err)
	}
	_, err = s.IssueTelegramLinkCode(ctx, "u1", linkCode("u1"), telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	linked, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{BotUsername: "ALICE_BROOD_BOT", Code: linkCode("u1"), ChatID: 42, TelegramUserID: 42, Now: telegramTestTime})
	if err != nil || linked.State != "linked" {
		t.Fatalf("code through the bot named in another case = %+v, %v", linked, err)
	}
	if !active(t, s, "u1", "Alice_Brood_Bot", 42, 42) {
		t.Fatal("update of the bot named in another case does not act through the link")
	}
	rotated, err := s.SetTelegramBot(ctx, "u1", "123456:ROTATED", "Alice_Brood_Bot", "")
	if err != nil || rotated.State != "linked" || rotated.BotUsername != "Alice_Brood_Bot" || !active(t, s, "u1", "alice_brood_bot", 42, 42) {
		t.Fatalf("new token of the same bot named in another case = %+v, %v; want the link kept", rotated, err)
	}
	// Другой бот остаётся другим.
	if active(t, s, "u1", "alice_brood_bot2", 42, 42) {
		t.Fatal("another bot acts through the link")
	}
}

// TestTelegramBotNamesCompareLikeTheSchema: Go сравнивает имена ботов так же,
// как COLLATE NOCASE колонки bot_username: без регистра только латиница.
// Иначе проверка в Go и UNIQUE схемы разошлись бы на одном имени.
func TestTelegramBotNamesCompareLikeTheSchema(t *testing.T) {
	s := newTestStore(t)
	for _, pair := range [][2]string{
		{"alice_bot", "alice_bot"},
		{"alice_bot", "Alice_Bot"},
		{"ALICE_BOT", "alice_bot"},
		{"alice_bot", "alice_bot2"},
		{"alice_bot", "alice-bot"},
		{"", ""},
		{"", "a"},
		{"@", "`"},
		{"[", "{"},
		{"_", "\x7f"},
		{"kelvin_bot", "\u212aelvin_bot"},
		{"\u00e9_bot", "\u00c9_bot"},
	} {
		var want bool
		mustNil(t, s.db.QueryRow(`SELECT ? = ? COLLATE NOCASE`, pair[0], pair[1]).Scan(&want))
		if got := telegramSameBot(pair[0], pair[1]); got != want {
			t.Errorf("telegramSameBot(%q, %q) = %v, COLLATE NOCASE says %v", pair[0], pair[1], got, want)
		}
	}
}

func TestTelegramLinkCodeIsOneTimeWithTTLAndBoundToTheBot(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	_, err := s.SetTelegramBot(ctx, "u1", "111:a", "alice_brood_bot", "")
	mustNil(t, err)
	issued, err := s.IssueTelegramLinkCode(ctx, "u1", linkCode("first"), telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	if issued.State != "unlinked" || issued.LinkCodeExpiresAt != "2026-11-13T12:01:00Z" {
		t.Fatalf("issued view = %+v", issued)
	}
	// Новый код заменяет прежний.
	_, err = s.IssueTelegramLinkCode(ctx, "u1", linkCode("second"), telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	confirm := func(bot, code string, now time.Time) error {
		_, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{BotUsername: bot, Code: code, ChatID: 42, TelegramUserID: 42, Now: now})
		return err
	}
	for name, err := range map[string]error{
		"replaced code":        confirm("alice_brood_bot", linkCode("first"), telegramTestTime),
		"unknown code":         confirm("alice_brood_bot", linkCode("third"), telegramTestTime),
		"expired code":         confirm("alice_brood_bot", linkCode("second"), telegramTestTime.Add(time.Minute)),
		"installation bot":     confirm("", linkCode("second"), telegramTestTime),
		"bot of another owner": confirm("other_bot", linkCode("second"), telegramTestTime),
	} {
		if !errors.Is(err, ErrTelegramLinkCode) {
			t.Fatalf("%s = %v, want ErrTelegramLinkCode", name, err)
		}
	}
	if link, _ := s.GetTelegramLink(ctx, "u1"); link.State != "unlinked" || telegramSessions(t, s) != nil {
		t.Fatalf("refused codes changed the link: %+v, sessions %v", link, telegramSessions(t, s))
	}
	mustNil(t, confirm("alice_brood_bot", linkCode("second"), telegramTestTime.Add(59*time.Second)))
	if err := confirm("alice_brood_bot", linkCode("second"), telegramTestTime); !errors.Is(err, ErrTelegramLinkCode) {
		t.Fatalf("reused code = %v, want ErrTelegramLinkCode", err)
	}
	if _, err := s.IssueTelegramLinkCode(ctx, "u1", linkCode("again"), telegramTestTime, telegramTestTime.Add(time.Minute)); !errors.Is(err, ErrTelegramLinked) {
		t.Fatalf("code for a linked user = %v, want ErrTelegramLinked", err)
	}
	// Код пользователя бота установки не принимает чужой бот.
	_, err = s.IssueTelegramLinkCode(ctx, "u2", linkCode("fallback"), telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	if err := confirm("alice_brood_bot", linkCode("fallback"), telegramTestTime); !errors.Is(err, ErrTelegramLinkCode) {
		t.Fatalf("fallback code through an own bot = %v, want ErrTelegramLinkCode", err)
	}
	for name, in := range map[string]TelegramLinkConfirmation{
		"empty code":             {ChatID: 7, TelegramUserID: 7, Now: telegramTestTime},
		"no chat":                {Code: linkCode("fallback"), TelegramUserID: 7, Now: telegramTestTime},
		"no sender":              {Code: linkCode("fallback"), ChatID: 7, Now: telegramTestTime},
		"group chat":             {Code: linkCode("fallback"), ChatID: -100123, TelegramUserID: 77, Now: telegramTestTime},
		"group of its sender":    {Code: linkCode("fallback"), ChatID: -77, TelegramUserID: -77, Now: telegramTestTime},
		"chat of another sender": {Code: linkCode("fallback"), ChatID: 7, TelegramUserID: 8, Now: telegramTestTime},
		"no clock":               {Code: linkCode("fallback"), ChatID: 7, TelegramUserID: 7},
	} {
		if _, err := s.ConfirmTelegramLink(ctx, in); !errors.Is(err, ErrTelegramInvalid) {
			t.Fatalf("%s = %v, want ErrTelegramInvalid", name, err)
		}
	}
	if _, err := s.IssueTelegramLinkCode(ctx, "u2", "", telegramTestTime, telegramTestTime.Add(time.Minute)); !errors.Is(err, ErrTelegramInvalid) {
		t.Fatalf("empty code issued: %v", err)
	}
	// Отключённому пользователю привязка не открывает сессию.
	mustNil(t, s.SetUserDisabled(ctx, "u2", true))
	if _, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{Code: linkCode("fallback"), ChatID: 7, TelegramUserID: 7, Now: telegramTestTime}); !errors.Is(err, ErrTelegramLinkCode) {
		t.Fatalf("link of a disabled user = %v, want ErrTelegramLinkCode", err)
	}
	if got := telegramSessions(t, s); !reflect.DeepEqual(got, []string{"u1:never"}) {
		t.Fatalf("refused links opened telegram sessions: %v", got)
	}
}

// TestTelegramLinkCodeTTLNeedsTheClock: срок кода сверяется с моментом,
// который называет вызывающий. Момент не назван — отказ, а не «всегда
// раньше срока»: иначе забытый Now принял бы любой истёкший код.
func TestTelegramLinkCodeTTLNeedsTheClock(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	// Код выдан час назад на минуту: к telegramTestTime он истёк.
	stale, err := s.IssueTelegramLinkCode(ctx, "u1", linkCode("stale"), telegramTestTime.Add(-time.Hour), telegramTestTime.Add(-time.Hour+time.Minute))
	mustNil(t, err)
	if _, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{Code: linkCode("stale"), ChatID: 42, TelegramUserID: 42}); !errors.Is(err, ErrTelegramInvalid) {
		t.Fatalf("expired code without a clock = %v, want ErrTelegramInvalid", err)
	}
	if _, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{Code: linkCode("stale"), ChatID: 42, TelegramUserID: 42, Now: telegramTestTime}); !errors.Is(err, ErrTelegramLinkCode) {
		t.Fatalf("expired code = %v, want ErrTelegramLinkCode", err)
	}
	// Код выдаётся на срок после момента выдачи и не дальше часа от него:
	// код, который живёт годами, ждал бы перебора.
	for name, in := range map[string][2]time.Time{
		// Неназванный момент выдачи — отказ и тогда, когда срок через минуту
		// после нулевого момента прошёл бы проверку срока.
		"no clock":               {{}, telegramTestTime.Add(time.Minute)},
		"only the clock missing": {{}, time.Time{}.Add(time.Minute)},
		"no expiry":              {telegramTestTime, {}},
		"expiry now":             {telegramTestTime, telegramTestTime},
		"expiry passed":          {telegramTestTime, telegramTestTime.Add(-time.Second)},
		"expiry beyond an hour":  {telegramTestTime, telegramTestTime.Add(time.Hour + time.Nanosecond)},
		"expiry in 100 years":    {telegramTestTime, telegramTestTime.AddDate(100, 0, 0)},
	} {
		if _, err := s.IssueTelegramLinkCode(ctx, "u1", linkCode("fresh"), in[0], in[1]); !errors.Is(err, ErrTelegramInvalid) {
			t.Fatalf("code with %s = %v, want ErrTelegramInvalid", name, err)
		}
	}
	link, err := s.GetTelegramLink(ctx, "u1")
	mustNil(t, err)
	if link.State != "unlinked" || link.LinkCodeExpiresAt != stale.LinkCodeExpiresAt || telegramSessions(t, s) != nil {
		t.Fatalf("refused codes changed the link: %+v, sessions %v", link, telegramSessions(t, s))
	}
	if issued, err := s.IssueTelegramLinkCode(ctx, "u1", linkCode("fresh"), telegramTestTime, telegramTestTime.Add(time.Hour)); err != nil || issued.LinkCodeExpiresAt != "2026-11-13T13:00:00Z" {
		t.Fatalf("code for exactly an hour = %+v, %v", issued, err)
	}
}

// TestExpiredLinkCodeFreesItsCode: истёкший код не держит своё значение: тот
// же код выдаётся другому пользователю, а прежний владелец его теряет.
func TestExpiredLinkCodeFreesItsCode(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	_, err := s.IssueTelegramLinkCode(ctx, "u1", linkCode("shared"), telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	if _, err := s.IssueTelegramLinkCode(ctx, "u2", linkCode("shared"), telegramTestTime.Add(59*time.Second), telegramTestTime.Add(time.Hour)); !errors.Is(err, ErrTelegramInvalid) || !strings.Contains(err.Error(), "already issued") {
		t.Fatalf("a live code issued twice = %v, want the already-issued refusal", err)
	}
	issued, err := s.IssueTelegramLinkCode(ctx, "u2", linkCode("shared"), telegramTestTime.Add(time.Minute), telegramTestTime.Add(time.Hour))
	if err != nil || issued.LinkCodeExpiresAt != "2026-11-13T13:00:00Z" {
		t.Fatalf("expired code issued again = %+v, %v", issued, err)
	}
	if first, err := s.GetTelegramLink(ctx, "u1"); err != nil || first.LinkCodeExpiresAt != "" || first.State != "unlinked" {
		t.Fatalf("previous holder keeps the expired code: %+v, %v", first, err)
	}
	linked, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{Code: linkCode("shared"), ChatID: 42, TelegramUserID: 42, Now: telegramTestTime.Add(2 * time.Minute)})
	if err != nil || linked.UserID != "u2" || linked.State != "linked" {
		t.Fatalf("code links its new holder = %+v, %v", linked, err)
	}
}

func TestTelegramSessionIsTiedToTheLink(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	link := linkTelegram(t, s, "u1", "", 42)
	if link.State != "linked" || link.ChatID != 42 || link.TelegramUserID != 42 || link.LinkCodeExpiresAt != "" {
		t.Fatalf("linked view = %+v", link)
	}
	if got := telegramSessions(t, s); !reflect.DeepEqual(got, []string{"u1:never"}) {
		t.Fatalf("telegram sessions = %v, want one without expiry", got)
	}
	var key string
	mustNil(t, s.db.QueryRow(`SELECT session_hash FROM user_telegram_links WHERE user_id = 'u1'`).Scan(&key))
	// Сессия telegram не cookie: веб-вход её не находит и не завершает.
	if _, err := s.GetSession(ctx, key); !isNotFound(err) {
		t.Fatalf("web lookup of the telegram session = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSession(ctx, key); !isNotFound(err) {
		t.Fatalf("web logout of the telegram session = %v, want ErrNotFound", err)
	}
	if !active(t, s, "u1", "", 42, 42) {
		t.Fatal("linked chat has no live session")
	}
	for name, in := range map[string]struct {
		user, bot  string
		chat, from int64
	}{
		"other chat":   {"u1", "", 43, 42},
		"group chat":   {"u1", "", -100123, 42},
		"other sender": {"u1", "", 42, 43},
		"other user":   {"u2", "", 42, 42},
		"other bot":    {"u1", "other_bot", 42, 42},
	} {
		if active(t, s, in.user, in.bot, in.chat, in.from) {
			t.Fatalf("%s acts through the link", name)
		}
	}
	unlinked, err := s.UnlinkTelegram(ctx, "u1")
	mustNil(t, err)
	if unlinked.State != "unlinked" || unlinked.ChatID != 0 || unlinked.TelegramUserID != 0 {
		t.Fatalf("unlinked view = %+v", unlinked)
	}
	if got := telegramSessions(t, s); got != nil || active(t, s, "u1", "", 42, 42) {
		t.Fatalf("unlink left the session: %v", got)
	}
	// Вид сессии — из словаря.
	if _, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, kind) VALUES ('other-kind', 'u1', 'now', 'other')`); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("session of an unknown kind: %v, want a CHECK refusal", err)
	}
	// Прямая запись: привязка без существующей сессии не бывает.
	if _, err := s.db.Exec(`UPDATE user_telegram_links SET state = 'linked', chat_id = 1, telegram_user_id = 1 WHERE user_id = 'u1'`); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("linked without a session: %v, want a CHECK refusal", err)
	}
	if _, err := s.db.Exec(`UPDATE user_telegram_links SET state = 'linked', chat_id = 1, telegram_user_id = 1, session_hash = 'missing' WHERE user_id = 'u1'`); !IsForeignKeyViolation(err) {
		t.Fatalf("linked to a missing session: %v, want a foreign key violation", err)
	}
}

// TestTelegramChatSessionNamesOneLink: живая привязка называет свою сессию,
// одну и ту же на каждом обновлении, и только для своих бота, чата и
// собеседника. Чат, отвязанный и привязанный снова, получает другую сессию:
// по ней команды чата (#284) отличают подтверждение прежней привязки.
func TestTelegramChatSessionNamesOneLink(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	linkTelegram(t, s, "u1", "", 42)
	first, err := s.TelegramChatSession(ctx, "u1", "", 42, 42)
	if err != nil || first == "" {
		t.Fatalf("session of a live link = %q, %v", first, err)
	}
	if again, err := s.TelegramChatSession(ctx, "u1", "", 42, 42); err != nil || again != first {
		t.Fatalf("session of the same link = %q, %v; want %q", again, err, first)
	}
	for name, in := range map[string]struct {
		user, bot  string
		chat, from int64
	}{
		"other chat":   {"u1", "", 43, 42},
		"other sender": {"u1", "", 42, 43},
		"other user":   {"u2", "", 42, 42},
		"other bot":    {"u1", "other_bot", 42, 42},
	} {
		if got, err := s.TelegramChatSession(ctx, in.user, in.bot, in.chat, in.from); err != nil || got != "" {
			t.Fatalf("%s: session = %q, %v; want none", name, got, err)
		}
	}
	_, err = s.UnlinkTelegram(ctx, "u1")
	mustNil(t, err)
	if got, err := s.TelegramChatSession(ctx, "u1", "", 42, 42); err != nil || got != "" {
		t.Fatalf("session of an unlinked chat = %q, %v", got, err)
	}
	linkTelegram(t, s, "u1", "", 42)
	second, err := s.TelegramChatSession(ctx, "u1", "", 42, 42)
	if err != nil || second == "" || second == first {
		t.Fatalf("session of the chat linked again = %q, %v; want a new one, not %q", second, err, first)
	}
	_, err = s.db.Exec(`UPDATE users SET disabled = 1 WHERE id = 'u1'`)
	mustNil(t, err)
	if got, err := s.TelegramChatSession(ctx, "u1", "", 42, 42); err != nil || got != "" {
		t.Fatalf("session of a disabled user = %q, %v", got, err)
	}
}

// TestTelegramSessionActiveNeedsTheLinksOwnTelegramSession: проверка в
// транзакции не верит одной строке привязки: пользователь, отключённый мимо
// хранилища, привязка, указывающая на веб-сессию, и привязка, указывающая на
// сессию telegram другого пользователя, права не дают. Выход из веб-сессии,
// на которую указывает такая привязка, проходит и снимает привязку с
// причиной отзыва: триггер отзыва смотрит на ссылку, а не на вид сессии.
func TestTelegramSessionActiveNeedsTheLinksOwnTelegramSession(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	linkTelegram(t, s, "u1", "", 42)
	_, err := s.db.Exec(`UPDATE users SET disabled = 1 WHERE id = 'u1'`)
	mustNil(t, err)
	if active(t, s, "u1", "", 42, 42) {
		t.Fatal("disabled user acts through Telegram")
	}
	_, err = s.db.Exec(`UPDATE users SET disabled = 0 WHERE id = 'u1'`)
	mustNil(t, err)
	if !active(t, s, "u1", "", 42, 42) {
		t.Fatal("enabled user lost the link")
	}
	mustNil(t, s.CreateSession(ctx, Session{TokenHash: "web-u1", UserID: "u1"}))
	_, err = s.db.Exec(`UPDATE user_telegram_links SET session_hash = 'web-u1' WHERE user_id = 'u1'`)
	mustNil(t, err)
	if active(t, s, "u1", "", 42, 42) {
		t.Fatal("a web session authorizes Telegram")
	}
	if err := s.DeleteSession(ctx, "web-u1"); err != nil {
		t.Fatalf("logout of a web session a link points at = %v", err)
	}
	if link, err := s.GetTelegramLink(ctx, "u1"); err != nil || link.State != "error" || link.Reason != telegramSessionRevoked || link.ChatID != 0 {
		t.Fatalf("link after its web session is deleted = %+v, %v", link, err)
	}
	linkTelegram(t, s, "u1", "", 42)
	_, err = s.db.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, kind) VALUES ('telegram:of-u2', 'u2', 'now', 'telegram')`)
	mustNil(t, err)
	_, err = s.db.Exec(`UPDATE user_telegram_links SET session_hash = 'telegram:of-u2' WHERE user_id = 'u1'`)
	mustNil(t, err)
	if active(t, s, "u1", "", 42, 42) {
		t.Fatal("a telegram session of another user authorizes the link")
	}
}

// TestTelegramSessionActiveIsBoundToTheLinksBot: привязка живёт только для
// обновлений бота, через который она сделана. Пользователь Telegram может
// написать и боту установки, и своему боту другого пользователя brood.
func TestTelegramSessionActiveIsBoundToTheLinksBot(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	_, err := s.SetTelegramBot(ctx, "u1", "111:a", "alice_brood_bot", "")
	mustNil(t, err)
	linkTelegram(t, s, "u1", "alice_brood_bot", 42)
	linkTelegram(t, s, "u2", "", 77)
	for name, in := range map[string]struct {
		user, bot string
		chat      int64
		want      bool
	}{
		"own bot":                         {"u1", "alice_brood_bot", 42, true},
		"installation bot for an own bot": {"u1", "", 42, false},
		"another bot for an own bot":      {"u1", "other_bot", 42, false},
		"installation bot":                {"u2", "", 77, true},
		"own bot of another user":         {"u2", "alice_brood_bot", 77, false},
	} {
		if got := active(t, s, in.user, in.bot, in.chat, in.chat); got != in.want {
			t.Fatalf("%s: active = %v, want %v", name, got, in.want)
		}
	}
}

// TestRevokingUserSessionsDropsTheTelegramLink: отзыв сессий пользователя —
// блокировка, смена роли в любую сторону, отзыв всех — снимает и привязку.
func TestRevokingUserSessionsDropsTheTelegramLink(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		admin  bool
		revoke func(s *Store) error
	}{
		"disable": {true, func(s *Store) error { return s.SetUserDisabled(ctx, "u1", true) }},
		"revoke all": {true, func(s *Store) error {
			_, err := s.DeleteSessionsOfUser(ctx, "u1")
			return err
		}},
		"demote": {true, func(s *Store) error { return s.SetUserAdmin(ctx, "u1", false) }},
		// Повышение тоже меняет полномочия сессий и отзывает их.
		"promote": {false, func(s *Store) error {
			if err := s.UpdateUserProperties(ctx, "u1", "Alice", true, false); err != nil {
				return err
			}
			if user, err := s.GetUser(ctx, "u1"); err != nil || !user.IsAdmin {
				return fmt.Errorf("promoted user = %+v, %v", user, err)
			}
			return nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s := telegramStore(t)
			seedUser(t, s, "root", "root")
			mustNil(t, s.SetUserAdmin(ctx, "root", true))
			mustNil(t, s.SetUserAdmin(ctx, "u1", tc.admin))
			linkTelegram(t, s, "u1", "", 42)
			mustNil(t, tc.revoke(s))
			link, err := s.GetTelegramLink(ctx, "u1")
			mustNil(t, err)
			if link.State != "error" || link.Reason != telegramSessionRevoked || link.ChatID != 0 {
				t.Fatalf("link after revocation = %+v", link)
			}
			if got := telegramSessions(t, s); got != nil || active(t, s, "u1", "", 42, 42) {
				t.Fatalf("revocation left the telegram session: %v", got)
			}
			if _, err := time.Parse(time.RFC3339Nano, link.UpdatedAt); err != nil || link.UpdatedAt < "2026" {
				t.Fatalf("revocation updated_at = %q, %v", link.UpdatedAt, err)
			}
			if name == "disable" {
				return
			}
			// Новая привязка после отзыва — обычная привязка с новым кодом.
			if relinked := linkTelegram(t, s, "u1", "", 42); relinked.State != "linked" || relinked.Reason != "" {
				t.Fatalf("relinked view = %+v", relinked)
			}
		})
	}
}

func TestTelegramAnswerRequiresTheLiveSessionInTheAnswerTransaction(t *testing.T) {
	ctx := context.Background()
	s, node := requestStore(t)
	viaTelegram := func(ctx context.Context) bool {
		ok, err := s.TelegramSessionActive(ctx, "u", "", 42, 42)
		return err == nil && ok
	}
	answerFromTelegram := func(ctx context.Context) error {
		_, err := s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "s", RequestID: node.ID, UserID: "u", Outcome: "success", Response: "ship"}, viaTelegram)
		return err
	}
	if err := answerFromTelegram(ctx); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("answer without a link = %v, want ErrSwarmRequestDenied", err)
	}
	linkTelegram(t, s, "u", "", 42)
	// Отвязка в той же транзакции раньше ответа отвергает ответ: проверка
	// читает транзакцию записи ответа, а не прежний снимок.
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.UnlinkTelegram(ctx, "u"); err != nil {
			return err
		}
		return answerFromTelegram(ctx)
	})
	if !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("answer after unlink in the transaction = %v, want ErrSwarmRequestDenied", err)
	}
	if got, _ := s.GetSwarmRequest(ctx, "s", node.ID); got.State != "open" {
		t.Fatalf("refused answer changed the request: %+v", got)
	}
	mustNil(t, answerFromTelegram(ctx))
	got, err := s.GetSwarmRequest(ctx, "s", node.ID)
	mustNil(t, err)
	if got.State != "answered" || got.AnsweredBy != "u" {
		t.Fatalf("answer from Telegram = %+v, want answered by the brood user", got)
	}
}

func TestTelegramBotChangeResetsOnlyAnotherBotsLink(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	_, err := s.SetTelegramBot(ctx, "u1", "111:a", "alice_brood_bot", "")
	mustNil(t, err)
	linkTelegram(t, s, "u1", "alice_brood_bot", 42)
	// Перевыпуск токена того же бота не трогает чат: бот и чат те же.
	rotated, err := s.SetTelegramBot(ctx, "u1", "111:rotated", "alice_brood_bot", "")
	mustNil(t, err)
	if rotated.State != "linked" || !active(t, s, "u1", "alice_brood_bot", 42, 42) {
		t.Fatalf("token rotation dropped the link: %+v", rotated)
	}
	other, err := s.SetTelegramBot(ctx, "u1", "222:b", "alice_other_bot", "")
	mustNil(t, err)
	if other.State != "unlinked" || other.ChatID != 0 || telegramSessions(t, s) != nil {
		t.Fatalf("another bot kept the link: %+v, sessions %v", other, telegramSessions(t, s))
	}
	linkTelegram(t, s, "u1", "alice_other_bot", 42)
	// Без своего бота привязка уходит к боту установки: прежний чат не годится.
	cleared, err := s.ClearTelegramBot(ctx, "u1")
	mustNil(t, err)
	if cleared.State != "unlinked" || telegramSessions(t, s) != nil {
		t.Fatalf("clearing the own bot kept the link: %+v", cleared)
	}
	// Ошибка привязки снимается новым токеном того же бота.
	_, err = s.SetTelegramBot(ctx, "u1", "222:c", "alice_other_bot", "")
	mustNil(t, err)
	failed, err := s.RecordTelegramLinkError(ctx, "u1", "bot token was rejected by Telegram", "")
	mustNil(t, err)
	if failed.State != "error" || failed.Reason != "bot token was rejected by Telegram" {
		t.Fatalf("recorded error = %+v", failed)
	}
	fixed, err := s.SetTelegramBot(ctx, "u1", "222:d", "alice_other_bot", "")
	mustNil(t, err)
	if fixed.State != "unlinked" || fixed.Reason != "" {
		t.Fatalf("new token kept the error: %+v", fixed)
	}
}

func TestTelegramLinkErrorDropsTheSession(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	linkTelegram(t, s, "u1", "", 42)
	failed, err := s.RecordTelegramLinkError(ctx, "u1", "the chat blocked the bot", "")
	mustNil(t, err)
	if failed.State != "error" || failed.Reason != "the chat blocked the bot" || failed.ChatID != 0 {
		t.Fatalf("error view = %+v", failed)
	}
	if telegramSessions(t, s) != nil || active(t, s, "u1", "", 42, 42) {
		t.Fatal("link error kept the telegram session")
	}
	if _, err := s.RecordTelegramLinkError(ctx, "u1", " ", ""); !errors.Is(err, ErrTelegramInvalid) {
		t.Fatalf("blank reason = %v, want ErrTelegramInvalid", err)
	}
	// Отвязка снимает и ошибку.
	if unlinked, err := s.UnlinkTelegram(ctx, "u1"); err != nil || unlinked.State != "unlinked" || unlinked.Reason != "" {
		t.Fatalf("unlink after an error = %+v, %v", unlinked, err)
	}
	for name, statement := range map[string]string{
		"error without reason":  `UPDATE user_telegram_links SET state = 'error' WHERE user_id = 'u1'`,
		"reason without error":  `UPDATE user_telegram_links SET reason = 'x' WHERE user_id = 'u1'`,
		"unknown state":         `UPDATE user_telegram_links SET state = 'paused' WHERE user_id = 'u1'`,
		"code without its TTL":  `UPDATE user_telegram_links SET link_code_hash = 'h' WHERE user_id = 'u1'`,
		"token without its bot": `UPDATE user_telegram_links SET bot_token = '1:x' WHERE user_id = 'u1'`,
		"events not a list":     `UPDATE user_telegram_links SET events = '{}' WHERE user_id = 'u1'`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// TestTelegramLinkSchemaKeepsTheLinkShape: CHECK держат форму привязки и при
// записи мимо хранилища; каждая запись нарушает ровно одно правило.
func TestTelegramLinkSchemaKeepsTheLinkShape(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	linkTelegram(t, s, "u1", "", 42)
	_, err := s.SetTelegramEvents(ctx, "u2", TelegramEventKinds())
	mustNil(t, err)
	for name, statement := range map[string]string{
		"linked without a chat":      `UPDATE user_telegram_links SET chat_id = NULL WHERE user_id = 'u1'`,
		"linked without a sender":    `UPDATE user_telegram_links SET telegram_user_id = NULL WHERE user_id = 'u1'`,
		"linked with a link code":    `UPDATE user_telegram_links SET link_code_hash = 'h', link_code_expires_at = '2026-11-13T12:10:00Z' WHERE user_id = 'u1'`,
		"group chat":                 `UPDATE user_telegram_links SET chat_id = -100, telegram_user_id = -100 WHERE user_id = 'u1'`,
		"another sender in the chat": `UPDATE user_telegram_links SET telegram_user_id = 77 WHERE user_id = 'u1'`,
		"chat while not linked":      `UPDATE user_telegram_links SET chat_id = 7 WHERE user_id = 'u2'`,
		"sender while not linked":    `UPDATE user_telegram_links SET telegram_user_id = 7 WHERE user_id = 'u2'`,
		"empty bot token":            `UPDATE user_telegram_links SET bot_token = '', bot_username = 'bob_brood_bot' WHERE user_id = 'u2'`,
		"empty bot username":         `UPDATE user_telegram_links SET bot_token = '1:x', bot_username = '' WHERE user_id = 'u2'`,
		"empty reason":               `UPDATE user_telegram_links SET state = 'error', reason = '' WHERE user_id = 'u2'`,
	} {
		if _, err := s.db.Exec(statement); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Fatalf("%s: %v, want a CHECK refusal", name, err)
		}
	}
}

// TestTelegramLinkErrorNeverCarriesABotToken: причина ошибки уходит в
// представление привязки и в API профиля, поэтому текст с токеном бота
// отвергается, а не хранится: с токеном своего бота или бота установки,
// целым или его секретом, с адресом Bot API в любом регистре, с любым
// токеном формы Bot API, как прежний токен своего бота, которого строка
// привязки уже не хранит, и под percent-кодированием до трёх слоёв, даже
// рядом с одиноким '%'.
func TestTelegramLinkErrorNeverCarriesABotToken(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	const own = "123456:SENTINEL-OWN-TOKEN"
	const installation = "999888:SENTINEL-INSTALLATION"
	const previous = "5550001234:AAH-SENTINEL-previous-own-token-01"
	_, err := s.SetTelegramBot(ctx, "u1", own, "alice_brood_bot", "")
	mustNil(t, err)
	linkTelegram(t, s, "u1", "alice_brood_bot", 42)
	before := databaseRows(t, s.db)
	for name, in := range map[string]struct{ user, reason string }{
		"own token in a Bot API URL":          {"u1", `Get "https://api.telegram.org/bot` + own + `/getMe": dial tcp: i/o timeout`},
		"own token in text":                   {"u1", "token " + own + " was rejected"},
		"secret of the own token":             {"u1", "Unauthorized: SENTINEL-OWN-TOKEN"},
		"installation token in a Bot API URL": {"u2", `Post "https://api.telegram.org/bot999:SENTINEL-INSTALLATION/sendMessage": EOF`},
		"installation token in text":          {"u2", "token " + installation + " was rejected"},
		"secret of the installation token":    {"u1", "secret SENTINEL-INSTALLATION expired"},
		"previous own token in text":          {"u1", "token " + previous + " rejected"},
		"upper-case Bot API URL":              {"u2", `Get "https://api.telegram.org/BOT777:SENTINEL-OTHER/getMe": EOF`},
		"percent-encoded bot id":              {"u2", `Get "https://api.telegram.org/bot%37%37%37:SENTINEL-OTHER/getMe": EOF`},
		"percent-encoded colon":               {"u2", `Get "https://api.telegram.org/bot777%3ASENTINEL-OTHER/getMe": EOF`},
		// Одинокий '%' рядом не мешает снять кодирование с токена.
		"own token next to a stray percent":          {"u1", `bot123456%3ASENTINEL-OWN-TOKEN/getMe": 100% failed`},
		"installation token next to a stray percent": {"u2", `bot999888%3ASENTINEL-INSTALLATION/sendMessage": 50% loss`},
		"lower-case escape next to a stray percent":  {"u1", `bot123456%3aSENTINEL-OWN-TOKEN/getMe": 100% failed`},
		"shaped token next to a stray percent":       {"u2", "token 5550001234%3AAAH-SENTINEL-previous-own-token-01 rejected: 100% failed"},
		"double-encoded colon":                       {"u1", `bot123456%253ASENTINEL-OWN-TOKEN/getMe": 100% failed`},
		"triple-encoded colon":                       {"u2", `Get "https://api.telegram.org/bot777%25253ASENTINEL-OTHER/getMe": EOF`},
		// Проверяется каждый слой, а не только последний: снятое кодирование
		// здесь приклеивает к секрету букву.
		"secret before an escape": {"u1", "Unauthorized: SENTINEL-OWN-TOKEN%2D1"},
	} {
		_, err := s.RecordTelegramLinkError(ctx, in.user, in.reason, installation)
		if !errors.Is(err, ErrTelegramInvalid) || strings.Contains(err.Error(), "SENTINEL") {
			t.Fatalf("%s = %v, want ErrTelegramInvalid without the token", name, err)
		}
	}
	if after := databaseRows(t, s.db); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused reasons changed the database:\n got %v\nwant %v", after, before)
	}
	// Причина без токена записывается, и слово, похожее на путь бота, ею
	// остаётся.
	for _, reason := range []string{"Forbidden: bot was blocked by the user", "robot42: blocked", "retry after 12:30", "100% of updates failed", "Bad Request: chat%20not%20found, 50% loss"} {
		failed, err := s.RecordTelegramLinkError(ctx, "u1", reason, installation)
		if err != nil || failed.State != "error" || failed.Reason != reason {
			t.Fatalf("reason without a token %q = %+v, %v", reason, failed, err)
		}
	}
}

// TestTelegramPercentDecodingTakesEachEscapeAlone: слой percent-кодирования
// снимается с каждого правильного %XX по отдельности, в любом регистре
// шестнадцатеричных цифр; одинокий или неправильный '%' остаётся как есть и
// не отменяет снятие остальных.
func TestTelegramPercentDecodingTakesEachEscapeAlone(t *testing.T) {
	for in, want := range map[string]string{
		"plain text":    "plain text",
		"100%":          "100%",
		"100% failed":   "100% failed",
		"%3A":           ":",
		"%3a":           ":",
		"%3A 100%":      ": 100%",
		"50% %3a":       "50% :",
		"%zz%3A":        "%zz:",
		"%3":            "%3",
		"%%3A":          "%:",
		"%253A":         "%3A",
		"%D0%B0%":       "\u0430%",
		"bot%37%37%37:": "bot777:",
	} {
		if got := telegramPercentDecode(in); got != want {
			t.Errorf("telegramPercentDecode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRefusedTelegramChangeInACallersTransactionWritesNothing: вложенный
// WithTx не открывает точку сохранения, поэтому вызывающий, обработавший
// отказ, зафиксировал бы частичную запись. Отказы проверяются до записи:
// подтверждение занятого чата не оставляет сессии, чужой бот не снимает
// привязку, а пользователь без строки привязки не получает строку по
// умолчанию.
func TestRefusedTelegramChangeInACallersTransactionWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	_, err := s.SetTelegramBot(ctx, "u1", "111:a", "alice_brood_bot", "")
	mustNil(t, err)
	linkTelegram(t, s, "u1", "alice_brood_bot", 42)
	_, err = s.SetTelegramBot(ctx, "u2", "222:b", "bob_brood_bot", "")
	mustNil(t, err)
	_, err = s.IssueTelegramLinkCode(ctx, "u2", linkCode("u2"), telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	seedUser(t, s, "u3", "carol")
	linkTelegram(t, s, "u3", "", 99)
	// У u4 строки привязки нет.
	seedUser(t, s, "u4", "dave")
	before := databaseRows(t, s.db)
	refusals := map[string]func(ctx context.Context) error{
		"bot of another user for a user without a link": func(ctx context.Context) error {
			_, err := s.SetTelegramBot(ctx, "u4", "444:d", "alice_brood_bot", "")
			if !errors.Is(err, ErrTelegramBotTaken) {
				return fmt.Errorf("set bot = %v, want ErrTelegramBotTaken", err)
			}
			return nil
		},
		"live code of another user for a user without a link": func(ctx context.Context) error {
			_, err := s.IssueTelegramLinkCode(ctx, "u4", linkCode("u2"), telegramTestTime, telegramTestTime.Add(time.Minute))
			if !errors.Is(err, ErrTelegramInvalid) {
				return fmt.Errorf("issue code = %v, want ErrTelegramInvalid", err)
			}
			return nil
		},
		"bot of another user in another case for a user without a link": func(ctx context.Context) error {
			_, err := s.SetTelegramBot(ctx, "u4", "444:d", "Alice_Brood_Bot", "")
			if !errors.Is(err, ErrTelegramBotTaken) {
				return fmt.Errorf("set bot = %v, want ErrTelegramBotTaken", err)
			}
			return nil
		},
		"installation bot for a user without a link": func(ctx context.Context) error {
			_, err := s.SetTelegramBot(ctx, "u4", "444:d", "brood_bot", "brood_bot")
			if !errors.Is(err, ErrTelegramBotTaken) {
				return fmt.Errorf("set bot = %v, want ErrTelegramBotTaken", err)
			}
			return nil
		},
		"code for a linked user": func(ctx context.Context) error {
			_, err := s.IssueTelegramLinkCode(ctx, "u3", linkCode("fresh"), telegramTestTime, telegramTestTime.Add(time.Minute))
			if !errors.Is(err, ErrTelegramLinked) {
				return fmt.Errorf("issue code = %v, want ErrTelegramLinked", err)
			}
			return nil
		},
		"reason carrying the own token": func(ctx context.Context) error {
			_, err := s.RecordTelegramLinkError(ctx, "u2", "token 222:b was rejected", "")
			if !errors.Is(err, ErrTelegramInvalid) {
				return fmt.Errorf("record error = %v, want ErrTelegramInvalid", err)
			}
			return nil
		},
		"confirm of a taken chat": func(ctx context.Context) error {
			_, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{BotUsername: "bob_brood_bot", Code: linkCode("u2"), ChatID: 42, TelegramUserID: 42, Now: telegramTestTime})
			if !errors.Is(err, ErrTelegramChatTaken) {
				return fmt.Errorf("confirm = %v, want ErrTelegramChatTaken", err)
			}
			return nil
		},
		"bot of another user": func(ctx context.Context) error {
			_, err := s.SetTelegramBot(ctx, "u3", "333:c", "alice_brood_bot", "")
			if !errors.Is(err, ErrTelegramBotTaken) {
				return fmt.Errorf("set bot = %v, want ErrTelegramBotTaken", err)
			}
			return nil
		},
	}
	for name, refuse := range refusals {
		if err := s.WithTx(ctx, refuse); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if after := databaseRows(t, s.db); !reflect.DeepEqual(after, before) {
			t.Fatalf("%s committed a partial write:\n got %v\nwant %v", name, after, before)
		}
	}
}

func TestTelegramEventSubscriptionsComeFromTheVocabulary(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	link, err := s.GetTelegramLink(ctx, "u1")
	mustNil(t, err)
	if !reflect.DeepEqual(link.Events, TelegramEventKinds()) || link.State != "unlinked" || link.HasBotToken {
		t.Fatalf("default link = %+v, want every event and no bot", link)
	}
	if _, err := s.GetTelegramLink(ctx, "ghost"); !isNotFound(err) {
		t.Fatalf("link of an unknown user = %v, want ErrNotFound", err)
	}
	set, err := s.SetTelegramEvents(ctx, "u1", []string{"pass_finished", "request_opened", "pass_finished"})
	mustNil(t, err)
	if want := []string{"request_opened", "pass_finished"}; !reflect.DeepEqual(set.Events, want) {
		t.Fatalf("events = %v, want %v in vocabulary order without duplicates", set.Events, want)
	}
	if got, _ := s.GetTelegramLink(ctx, "u1"); !reflect.DeepEqual(got.Events, set.Events) {
		t.Fatalf("stored events = %v", got.Events)
	}
	none, err := s.SetTelegramEvents(ctx, "u1", nil)
	mustNil(t, err)
	if none.Events == nil || len(none.Events) != 0 {
		t.Fatalf("no subscriptions = %#v, want an empty list", none.Events)
	}
	_, err = s.SetTelegramEvents(ctx, "u1", []string{"request_opend"})
	if !errors.Is(err, ErrTelegramInvalid) || !strings.Contains(err.Error(), strings.Join(TelegramEventKinds(), ", ")) {
		t.Fatalf("misspelled event = %v, want a refusal listing %v", err, TelegramEventKinds())
	}
	// Первая запись привязки заводит строку подписанной на всё, как и
	// представление без строки: настройка бота или кода не отписывает.
	seedUser(t, s, "u3", "carol")
	_, err = s.SetTelegramBot(ctx, "u2", "222:b", "bob_brood_bot", "")
	mustNil(t, err)
	_, err = s.IssueTelegramLinkCode(ctx, "u3", linkCode("u3"), telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	for _, user := range []string{"u2", "u3"} {
		var stored string
		mustNil(t, s.db.QueryRow(`SELECT events FROM user_telegram_links WHERE user_id = ?`, user).Scan(&stored))
		if got, _ := s.GetTelegramLink(ctx, user); !reflect.DeepEqual(got.Events, TelegramEventKinds()) {
			t.Fatalf("first write of %s left events %v (stored %s), want every event", user, got.Events, stored)
		}
	}
}

func TestDeletingAUserDeletesItsTelegramRows(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	linkTelegram(t, s, "u1", "", 42)
	// Уведомления не удаляются, пока жив пользователь, но уходят вместе с ним,
	// и отправленные, и неизвестного исхода.
	queueNotification(t, s, "u1", "request_opened")
	sent := queueNotification(t, s, "u1", "pass_started")
	outboxSteps(t, s, sent, `attempts = 1`, `state = 'sent', settled_attempts = 1, telegram_message_id = 1001`)
	unconfirmed := queueNotification(t, s, "u1", "pass_finished")
	outboxSteps(t, s, unconfirmed, `attempts = 1`, `state = 'unconfirmed', settled_attempts = 1`)
	mustNil(t, s.DeleteUser(ctx, "u1"))
	var n int
	mustNil(t, s.db.QueryRow(`SELECT (SELECT count(*) FROM user_telegram_links) + (SELECT count(*) FROM telegram_outbox) + (SELECT count(*) FROM sessions)`).Scan(&n))
	if n != 0 {
		t.Fatalf("rows left after deleting the user: %d", n)
	}
}

// TestTelegramInputsHaveLimits: токен бота и код привязки — слово до 256 байт
// без пробелов и управляющих символов, выдаваемый код не короче 16 байт, имя
// бота из getMe — до 64 латинских букв, цифр и подчёркиваний, причина ошибки
// — до 512 байт. Пределы держат и функции разбора, и методы хранилища.
func TestTelegramInputsHaveLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		want  bool
	}{
		"token":          {"123456:AAH-x_y", true},
		"at the limit":   {strings.Repeat("a", 256), true},
		"over the limit": {strings.Repeat("a", 257), false},
		"empty":          {"", false},
		"space":          {"a b", false},
		"tab":            {"a\tb", false},
		"newline":        {"a\nb", false},
		"NUL":            {"a\x00b", false},
		"unit separator": {"a\x1fb", false},
		"DEL":            {"a\x7fb", false},
	} {
		if got := telegramWord(tc.value, 256); got != tc.want {
			t.Errorf("telegramWord(%s) = %v, want %v", name, got, tc.want)
		}
	}
	for name, tc := range map[string]struct {
		value string
		want  bool
	}{
		"letters, digits and underscores": {"Alice_Brood_Bot_9", true},
		"at the limit":                    {strings.Repeat("b", 64), true},
		"over the limit":                  {strings.Repeat("b", 65), false},
		"empty":                           {"", false},
		"at sign":                         {"@alice_bot", false},
		"dash":                            {"alice-bot", false},
		"dot":                             {"alice.bot", false},
		"space":                           {"alice bot", false},
		"DEL":                             {"alice\x7fbot", false},
		"Cyrillic":                        {"алиса_bot", false},
	} {
		if got := TelegramBotName(tc.value); got != tc.want {
			t.Errorf("TelegramBotName(%s) = %v, want %v", name, got, tc.want)
		}
	}
	ctx := context.Background()
	s := telegramStore(t)
	token, bot, code := strings.Repeat("t", 256), strings.Repeat("b", 64), strings.Repeat("c", 256)
	for name, err := range map[string]error{
		"token over the limit":    second(s.SetTelegramBot(ctx, "u1", token+"t", bot, "")),
		"bot name over the limit": second(s.SetTelegramBot(ctx, "u1", token, bot+"b", "")),
		"code over the limit":     second(s.IssueTelegramLinkCode(ctx, "u1", code+"c", telegramTestTime, telegramTestTime.Add(time.Minute))),
		"code under the minimum":  second(s.IssueTelegramLinkCode(ctx, "u1", strings.Repeat("c", 15), telegramTestTime, telegramTestTime.Add(time.Minute))),
		"one-byte code":           second(s.IssueTelegramLinkCode(ctx, "u1", "c", telegramTestTime, telegramTestTime.Add(time.Minute))),
		"code of a confirmation over the limit": second(s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{
			BotUsername: bot, Code: code + "c", ChatID: 42, TelegramUserID: 42, Now: telegramTestTime})),
		"reason over the limit": second(s.RecordTelegramLinkError(ctx, "u2", strings.Repeat("r", 513), "")),
	} {
		if !errors.Is(err, ErrTelegramInvalid) {
			t.Fatalf("%s = %v, want ErrTelegramInvalid", name, err)
		}
	}
	_, err := s.SetTelegramBot(ctx, "u1", token, bot, "")
	mustNil(t, err)
	shortest := strings.Repeat("s", 16)
	_, err = s.IssueTelegramLinkCode(ctx, "u2", shortest, telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	if linked, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{Code: shortest, ChatID: 77, TelegramUserID: 77, Now: telegramTestTime}); err != nil || linked.State != "linked" {
		t.Fatalf("link with a code at the minimum = %+v, %v", linked, err)
	}
	_, err = s.IssueTelegramLinkCode(ctx, "u1", code, telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	linked, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{BotUsername: bot, Code: code, ChatID: 42, TelegramUserID: 42, Now: telegramTestTime})
	if err != nil || linked.State != "linked" {
		t.Fatalf("link at the limits = %+v, %v", linked, err)
	}
	failed, err := s.RecordTelegramLinkError(ctx, "u2", strings.Repeat("r", 512), "")
	if err != nil || len(failed.Reason) != 512 {
		t.Fatalf("reason at the limit = %+v, %v", failed, err)
	}
}

// second: ошибка из пары «значение, ошибка».
func second[T any](_ T, err error) error { return err }

// TestDroppingALinkClearsItsWaitingCode: отвязка, переход на бот установки,
// смена бота и ошибка привязки снимают ждущий код: прежний код больше ничего
// не привязывает. Новый код после ошибки возвращает привязку в unlinked без
// причины ещё до подтверждения.
func TestDroppingALinkClearsItsWaitingCode(t *testing.T) {
	ctx := context.Background()
	for name, drop := range map[string]func(s *Store) (TelegramLink, error){
		"unlink":            func(s *Store) (TelegramLink, error) { return s.UnlinkTelegram(ctx, "u1") },
		"clear the own bot": func(s *Store) (TelegramLink, error) { return s.ClearTelegramBot(ctx, "u1") },
		"another bot": func(s *Store) (TelegramLink, error) {
			return s.SetTelegramBot(ctx, "u1", "222:b", "alice_other_bot", "")
		},
		"link error": func(s *Store) (TelegramLink, error) {
			return s.RecordTelegramLinkError(ctx, "u1", "bot token was rejected by Telegram", "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := telegramStore(t)
			_, err := s.SetTelegramBot(ctx, "u1", "111:a", "alice_brood_bot", "")
			mustNil(t, err)
			waiting, err := s.IssueTelegramLinkCode(ctx, "u1", linkCode("waiting"), telegramTestTime, telegramTestTime.Add(time.Hour))
			mustNil(t, err)
			if waiting.LinkCodeExpiresAt == "" {
				t.Fatalf("issued code has no expiry: %+v", waiting)
			}
			dropped, err := drop(s)
			mustNil(t, err)
			if dropped.LinkCodeExpiresAt != "" {
				t.Fatalf("%s kept the waiting code: %+v", name, dropped)
			}
			// Код предъявлен боту, через который теперь идёт привязка.
			if _, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{BotUsername: dropped.BotUsername, Code: linkCode("waiting"), ChatID: 42, TelegramUserID: 42, Now: telegramTestTime}); !errors.Is(err, ErrTelegramLinkCode) {
				t.Fatalf("code after %s = %v, want ErrTelegramLinkCode", name, err)
			}
		})
	}
	s := telegramStore(t)
	linkTelegram(t, s, "u1", "", 42)
	_, err := s.RecordTelegramLinkError(ctx, "u1", "the chat blocked the bot", "")
	mustNil(t, err)
	issued, err := s.IssueTelegramLinkCode(ctx, "u1", linkCode("again"), telegramTestTime, telegramTestTime.Add(time.Minute))
	if err != nil || issued.State != "unlinked" || issued.Reason != "" || issued.LinkCodeExpiresAt == "" {
		t.Fatalf("code after a link error = %+v, %v; want unlinked without the reason", issued, err)
	}
}
