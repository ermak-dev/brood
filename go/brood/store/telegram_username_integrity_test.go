package store

import (
	"context"
	"errors"
	"testing"
)

// FEATURE: Имя собеседника и отвязка между чтением и записью (#294).
// SPEC: NoteTelegramUsername читает имя живой привязки без блокировки записи,
// а запись повторяет условие живой привязки. Чат, отвязанный после чтения,
// имени не получает, и вызывающий узнаёт ErrTelegramUnlinked, а не успех.
// IMPLEMENTATION: NoteTelegramUsername (telegram_links.go).
// STATUS: DONE

// Окно между чтением и записью воспроизводит триггер: в момент записи имени
// он отвязывает чат и пропускает запись, как пропустило бы её условие записи
// после настоящей отвязки.
func TestTelegramUsernameOfAChatUnlinkedBeforeTheWriteIsRefused(t *testing.T) {
	ctx := context.Background()
	s := telegramStore(t)
	linkTelegramAs(t, s, "u1", "", 42, "anton")
	session := chatSession(t, s, "u1", 42)
	if _, err := s.db.Exec(`CREATE TRIGGER unlink_before_username_write BEFORE UPDATE OF telegram_username ON user_telegram_links
		WHEN NEW.telegram_username IS NOT NULL
		BEGIN
			UPDATE user_telegram_links SET state = 'unlinked', chat_id = NULL, telegram_user_id = NULL, telegram_username = NULL, session_hash = NULL
				WHERE user_id = OLD.user_id;
			SELECT RAISE(IGNORE);
		END`); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteTelegramUsername(ctx, "u1", session, "anton_renamed"); !errors.Is(err, ErrTelegramUnlinked) {
		t.Fatalf("username of a chat unlinked before the write = %v, want ErrTelegramUnlinked", err)
	}
	if link, err := s.GetTelegramLink(ctx, "u1"); err != nil || link.State != "unlinked" || link.TelegramUsername != "" || storedTelegramUsername(t, s, "u1") != "<null>" {
		t.Fatalf("link unlinked before the write = %+v, %v; want it unlinked without a username", link, err)
	}
}
