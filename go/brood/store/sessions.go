// Сессии сервера: запись по хэшу токена. Физическая схема
// предварительная, аутентификация (#30) может адаптировать её под
// библиотеку сессий, не меняя остальной Store. Вид сессии (#279): web —
// вход браузера по cookie, его создают, ищут и завершают методы ниже;
// telegram открывает и закрывает привязка Telegram (telegram_links.go),
// cookie у неё нет. Отзыв сессий пользователя и уборка истёкших касаются
// обоих видов.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Session — строка таблицы sessions. TokenHash — хэш токена сессии,
// сам токен в базе не хранится.
type Session struct {
	TokenHash string
	UserID    string
	CreatedAt *string
	ExpiresAt *time.Time
	Data      []byte
}

// CreateSession inserts a web session; the store sets created_at, and a nil
// ExpiresAt leaves the session without expiry (NULL).
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	var expires any
	if sess.ExpiresAt != nil {
		expires = sess.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	// Проверка активности в той же записи SQL: конкурирующая блокировка
	// либо удалит эту сессию, либо не даст вставить её после отзыва.
	res, err := s.conn(ctx).ExecContext(ctx, `
		INSERT INTO sessions (token_hash, user_id, created_at, expires_at, data)
		SELECT ?, ?, ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM users WHERE id = ? AND disabled = 1)`,
		sess.TokenHash, sess.UserID, nowRFC3339(), expires, sess.Data, sess.UserID)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("cannot create session for disabled user")
	}
	return nil
}

// GetSession returns the web session with the token hash. A telegram session
// has no cookie and is never found here.
func (s *Store) GetSession(ctx context.Context, tokenHash string) (Session, error) {
	var sess Session
	var createdAt string
	var expires sql.NullString
	err := s.conn(ctx).QueryRowContext(ctx,
		`SELECT token_hash, user_id, created_at, expires_at, data FROM sessions WHERE token_hash = ? AND kind = 'web'`,
		tokenHash).Scan(&sess.TokenHash, &sess.UserID, &createdAt, &expires, &sess.Data)
	if err == sql.ErrNoRows {
		return sess, fmt.Errorf("%w: session", ErrNotFound)
	}
	if err != nil {
		return sess, err
	}
	sess.CreatedAt = &createdAt
	if expires.Valid {
		t, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil {
			return sess, fmt.Errorf("parse expires_at of session: %w", err)
		}
		sess.ExpiresAt = &t
	}
	return sess, nil
}

// DeleteSession deletes the web session with the token hash (logout).
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	res, err := s.conn(ctx).ExecContext(ctx,
		`DELETE FROM sessions WHERE token_hash = ? AND kind = 'web'`, tokenHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: session", ErrNotFound)
	}
	return nil
}

// DeleteSessionsOfUser revokes every session of the user, web and telegram
// (disable, a role change either way, password reset), and returns their
// number. Revoking a telegram session drops its Telegram link with a reason
// (sessions_telegram_revoke).
func (s *Store) DeleteSessionsOfUser(ctx context.Context, userID string) (int64, error) {
	res, err := s.conn(ctx).ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteExpiredSessions удаляет сессии, чей срок раньше now; сессии
// без срока не трогает. Возвращает число удалённых.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.conn(ctx).ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at IS NOT NULL AND expires_at < ?`,
		now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
