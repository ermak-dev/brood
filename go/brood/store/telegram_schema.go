package store

import (
	"context"
	"database/sql"
)

const userTelegramLinksSchema = `CREATE TABLE user_telegram_links (
    user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    bot_token TEXT CHECK (length(bot_token) > 0),
    bot_username TEXT UNIQUE COLLATE NOCASE CHECK (length(bot_username) > 0),
    chat_id INTEGER UNIQUE CHECK (chat_id > 0),
    telegram_user_id INTEGER,
    link_code_hash TEXT UNIQUE,
    link_code_expires_at TEXT,
    session_hash TEXT UNIQUE REFERENCES sessions(token_hash),
    state TEXT NOT NULL CHECK (state IN ('unlinked', 'linked', 'error')),
    reason TEXT CHECK (length(reason) > 0),
    updated_at TEXT NOT NULL,
    default_swarm_id TEXT REFERENCES swarms(id) ON DELETE SET NULL,
    telegram_username TEXT CHECK (telegram_username IS NULL OR (state = 'linked' AND length(telegram_username) BETWEEN 1 AND 32 AND telegram_username NOT GLOB '*[^A-Za-z0-9_]*')),
    CHECK ((bot_token IS NULL) = (bot_username IS NULL)),
    CHECK ((link_code_hash IS NULL) = (link_code_expires_at IS NULL)),
    CHECK ((state = 'linked') = (session_hash IS NOT NULL)),
    CHECK ((state = 'linked') = (chat_id IS NOT NULL)),
    CHECK ((state = 'linked') = (telegram_user_id IS NOT NULL)),
    CHECK (chat_id = telegram_user_id),
    CHECK (state <> 'linked' OR link_code_hash IS NULL),
    CHECK ((state = 'error') = (reason IS NOT NULL))
)`

const telegramSessionRevoked = "telegram session was revoked: link the chat again"

// sessionTelegramRevoke: удаление сессии, на которую ссылается привязка,
// снимает привязку с причиной. Так блокировка, смена роли и смена пароля
// (DeleteSessionsOfUser) отзывают и право бота, а внешний ключ привязки не
// мешает удалению. Триггер на любом виде сессии: привязка, которую запись
// мимо хранилища направила на веб-сессию, иначе запретила бы её удаление.
// Отвязка сначала снимает ссылку сама, и триггер её не находит. Имя
// собеседника снимается вместе с чатом (#294).
const sessionTelegramRevoke = `CREATE TRIGGER sessions_telegram_revoke BEFORE DELETE ON sessions BEGIN UPDATE user_telegram_links SET state = 'error', reason = '` + telegramSessionRevoked + `', chat_id = NULL, telegram_user_id = NULL, telegram_username = NULL, session_hash = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE session_hash = OLD.token_hash; END`

func telegramFullSchema() string {
	return userTelegramLinksSchema + ";" + sessionTelegramRevoke
}

func verifyTelegramSchema(ctx context.Context, tx *sql.Tx) error {
	if err := checkSchema(ctx, tx, userTelegramLinksSchema); err != nil {
		return err
	}
	return checkObject(ctx, tx, "trigger", "sessions_telegram_revoke", sessionTelegramRevoke)
}
