package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// FEATURE: Telegram username of the linked chat (#294, epic #278).
// SPEC: the link keeps the username its chat's sender carried in the
// confirmed link code and then the one each update of that live link brings,
// so it survives a server restart; a sender without a username has none. A
// username is 1 to 32 Latin letters, digits and underscores, without @. An
// update of an earlier link of the chat, or of no live link, writes nothing.
// Unlinking, a link error, a bot change and revoking the link's session clear
// it, and the schema refuses a username outside a live link or out of shape
// written past the store.
// IMPLEMENTATION: user_telegram_links.telegram_username and the trigger
// sessions_telegram_revoke (telegram_schema.go), ConfirmTelegramLink and
// NoteTelegramUsername (telegram_links.go).
// STATUS: DONE

// storedTelegramUsername — значение колонки имени привязки user; NULL —
// "<null>".
func storedTelegramUsername(t *testing.T, s *Store, user string) string {
	t.Helper()
	var name string
	mustNil(t, s.db.QueryRow(`SELECT coalesce(telegram_username, '<null>') FROM user_telegram_links WHERE user_id = ?`, user).Scan(&name))
	return name
}

// chatSession — сессия живой привязки user через бот установки в чате chat.
func chatSession(t *testing.T, s *Store, user string, chat int64) string {
	t.Helper()
	session, err := s.TelegramChatSession(context.Background(), user, "", chat, chat)
	if err != nil || session == "" {
		t.Fatalf("session of the link of %s = %q, %v", user, session, err)
	}
	return session
}

func TestTelegramLinkKeepsTheSendersUsername(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	if linked := linkTelegramAs(t, s, "u1", "", 42, "anton"); linked.TelegramUsername != "anton" {
		t.Fatalf("confirmed link = %+v, want the sender's username", linked)
	}
	if link, err := s.GetTelegramLink(ctx, "u1"); err != nil || link.TelegramUsername != "anton" {
		t.Fatalf("stored link = %+v, %v", link, err)
	}
	session := chatSession(t, s, "u1", 42)
	// Собеседник сменил имя: новое имя заменяет прежнее.
	mustNil(t, s.NoteTelegramUsername(ctx, "u1", session, "Anton_Renamed_9"))
	if link, err := s.GetTelegramLink(ctx, "u1"); err != nil || link.TelegramUsername != "Anton_Renamed_9" {
		t.Fatalf("link after a rename = %+v, %v", link, err)
	}
	// То же имя ничего не пишет: обновление приходит с каждым сообщением.
	before, err := s.GetTelegramLink(ctx, "u1")
	mustNil(t, err)
	mustNil(t, s.NoteTelegramUsername(ctx, "u1", session, "Anton_Renamed_9"))
	if after, err := s.GetTelegramLink(ctx, "u1"); err != nil || after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("the same username rewrote the link: %+v, %v; was %+v", after, err, before)
	}
	// Собеседник снял имя: у привязки его больше нет.
	mustNil(t, s.NoteTelegramUsername(ctx, "u1", session, ""))
	if link, err := s.GetTelegramLink(ctx, "u1"); err != nil || link.TelegramUsername != "" || storedTelegramUsername(t, s, "u1") != "<null>" {
		t.Fatalf("link after the username was removed = %+v, %v", link, err)
	}
	// Собеседник без имени привязывается без имени.
	if linked := linkTelegramAs(t, s, "u2", "", 77, ""); linked.State != "linked" || linked.TelegramUsername != "" || storedTelegramUsername(t, s, "u2") != "<null>" {
		t.Fatalf("link of a sender without a username = %+v", linked)
	}
}

// TestTelegramUsernameNeedsTheLiveLinksSession: имя пишет только обновление
// живой привязки, пришедшее под её сессией. Сессия прежней привязки того же
// чата, чужая сессия и пустая ничего не пишут.
func TestTelegramUsernameNeedsTheLiveLinksSession(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	linkTelegramAs(t, s, "u1", "", 42, "anton")
	earlier := chatSession(t, s, "u1", 42)
	_, err := s.UnlinkTelegram(ctx, "u1")
	mustNil(t, err)
	if err := s.NoteTelegramUsername(ctx, "u1", earlier, "stale"); !errors.Is(err, ErrTelegramUnlinked) {
		t.Fatalf("username of an unlinked chat = %v, want ErrTelegramUnlinked", err)
	}
	linkTelegramAs(t, s, "u1", "", 42, "anton2")
	linkTelegramAs(t, s, "u2", "", 77, "bob")
	for name, session := range map[string]string{
		"earlier link of the chat": earlier,
		"another user's session":   chatSession(t, s, "u2", 77),
		"no session":               "",
	} {
		if err := s.NoteTelegramUsername(ctx, "u1", session, "stale"); !errors.Is(err, ErrTelegramUnlinked) {
			t.Fatalf("%s: %v, want ErrTelegramUnlinked", name, err)
		}
	}
	if link, err := s.GetTelegramLink(ctx, "u1"); err != nil || link.TelegramUsername != "anton2" {
		t.Fatalf("link after refused usernames = %+v, %v", link, err)
	}
	if link, err := s.GetTelegramLink(ctx, "u2"); err != nil || link.TelegramUsername != "bob" {
		t.Fatalf("another user's link after refused usernames = %+v, %v", link, err)
	}
}

// TestTelegramUsernameHasTheTelegramShape: имя — до 32 латинских букв, цифр
// и подчёркиваний без @; иное хранилище отвергает и при подтверждении кода, и
// при обновлении, ничего не записав.
func TestTelegramUsernameHasTheTelegramShape(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		want  bool
	}{
		"letters, digits and underscores": {"Anton_Ermak_9", true},
		"one letter":                      {"a", true},
		"at the limit":                    {strings.Repeat("a", 32), true},
		"over the limit":                  {strings.Repeat("a", 33), false},
		"empty":                           {"", false},
		"at sign":                         {"@anton", false},
		"dash":                            {"an-ton", false},
		"dot":                             {"an.ton", false},
		"space":                           {"an ton", false},
		"DEL":                             {"an\x7fton", false},
		"Cyrillic":                        {"антон", false},
	} {
		if got := ValidTelegramUsername(tc.value); got != tc.want {
			t.Errorf("ValidTelegramUsername(%s) = %v, want %v", name, got, tc.want)
		}
	}
	ctx := context.Background()
	s := telegramStore(t)
	_, err := s.IssueTelegramLinkCode(ctx, "u1", linkCode("u1"), telegramTestTime, telegramTestTime.Add(time.Minute))
	mustNil(t, err)
	before := databaseRows(t, s.db)
	if _, err := s.ConfirmTelegramLink(ctx, TelegramLinkConfirmation{Code: linkCode("u1"), ChatID: 42, TelegramUserID: 42, Username: "@anton", Now: telegramTestTime}); !errors.Is(err, ErrTelegramInvalid) {
		t.Fatalf("confirmation with a malformed username = %v, want ErrTelegramInvalid", err)
	}
	if after := databaseRows(t, s.db); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused confirmation changed the database:\n got %v\nwant %v", after, before)
	}
	linkTelegramAs(t, s, "u1", "", 42, "anton")
	for _, malformed := range []string{"@anton", strings.Repeat("a", 33), "an ton"} {
		if err := s.NoteTelegramUsername(ctx, "u1", chatSession(t, s, "u1", 42), malformed); !errors.Is(err, ErrTelegramInvalid) {
			t.Fatalf("username %q = %v, want ErrTelegramInvalid", malformed, err)
		}
	}
	if link, err := s.GetTelegramLink(ctx, "u1"); err != nil || link.TelegramUsername != "anton" {
		t.Fatalf("link after malformed usernames = %+v, %v", link, err)
	}
}

// TestDroppingATelegramLinkClearsTheUsername: имя принадлежит живой
// привязке: отвязка, ошибка, смена бота, переход на бот установки и отзыв
// сессии привязки его снимают.
func TestDroppingATelegramLinkClearsTheUsername(t *testing.T) {
	ctx := context.Background()
	for name, drop := range map[string]func(s *Store) error{
		"unlink": func(s *Store) error { return second(s.UnlinkTelegram(ctx, "u1")) },
		"link error": func(s *Store) error {
			return second(s.RecordTelegramLinkError(ctx, "u1", "the chat blocked the bot", ""))
		},
		"another bot":       func(s *Store) error { return second(s.SetTelegramBot(ctx, "u1", "222:b", "alice_other_bot", "")) },
		"clear the own bot": func(s *Store) error { return second(s.ClearTelegramBot(ctx, "u1")) },
		"session revocation": func(s *Store) error {
			_, err := s.DeleteSessionsOfUser(ctx, "u1")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := telegramStore(t)
			_, err := s.SetTelegramBot(ctx, "u1", "111:a", "alice_brood_bot", "")
			mustNil(t, err)
			linkTelegramAs(t, s, "u1", "alice_brood_bot", 42, "anton")
			mustNil(t, drop(s))
			link, err := s.GetTelegramLink(ctx, "u1")
			mustNil(t, err)
			if link.State == "linked" || link.TelegramUsername != "" || storedTelegramUsername(t, s, "u1") != "<null>" {
				t.Fatalf("link after %s = %+v; want it dropped without the username", name, link)
			}
		})
	}
}

// TestTelegramUsernameSchemaKeepsItsShape: CHECK держит имя и при записи мимо
// хранилища: только у живой привязки и только формы имени Telegram. Каждая
// запись нарушает ровно это правило.
func TestTelegramUsernameSchemaKeepsItsShape(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	linkTelegramAs(t, s, "u1", "", 42, "anton")
	_, err := s.SetTelegramEvents(ctx, "u2", TelegramEventKinds())
	mustNil(t, err)
	for name, statement := range map[string]string{
		"username while not linked": `UPDATE user_telegram_links SET telegram_username = 'bob' WHERE user_id = 'u2'`,
		"unlinked keeping the username": `UPDATE user_telegram_links SET state = 'unlinked', chat_id = NULL, telegram_user_id = NULL, session_hash = NULL
			WHERE user_id = 'u1'`,
		"empty username": `UPDATE user_telegram_links SET telegram_username = '' WHERE user_id = 'u1'`,
		"at sign":        `UPDATE user_telegram_links SET telegram_username = '@anton' WHERE user_id = 'u1'`,
		"over the limit": `UPDATE user_telegram_links SET telegram_username = '` + strings.Repeat("a", 33) + `' WHERE user_id = 'u1'`,
		"space":          `UPDATE user_telegram_links SET telegram_username = 'an ton' WHERE user_id = 'u1'`,
		"Cyrillic":       `UPDATE user_telegram_links SET telegram_username = 'антон' WHERE user_id = 'u1'`,
	} {
		if _, err := s.db.Exec(statement); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Fatalf("%s: %v, want a CHECK refusal", name, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE user_telegram_links SET telegram_username = ? WHERE user_id = 'u1'`, strings.Repeat("a", 32)); err != nil {
		t.Fatalf("username at the limit written directly: %v", err)
	}
}
