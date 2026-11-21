// Серверные сессии в таблице sessions хранилища: единственный источник
// правды, никакого параллельного механизма хранения. Cookie держит
// случайный токен, база — его SHA-256 и CSRF-секрет сессии.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"brood.dev/go/brood/store"
)

const (
	// CookieSession — cookie сессии, HttpOnly.
	CookieSession = "session"
	// CookieCSRF — cookie CSRF, доступна JS.
	CookieCSRF = "csrf"
	// HeaderCSRF — заголовок изменяющих запросов.
	HeaderCSRF = "X-CSRFToken"
)

// DefaultSessionTTL — срок жизни сессии по умолчанию.
const DefaultSessionTTL = 30 * 24 * time.Hour

// ErrSessionNotFound — сессии нет, истекла или отозвана.
var ErrSessionNotFound = errors.New("session not found")

// sessionData — JSON в колонке data: CSRF-секрет сессии.
type sessionData struct {
	CSRF string `json:"csrf"`
}

// Sessions — создание, поиск и отзыв сессий поверх хранилища.
type Sessions struct {
	store *store.Store
	ttl   time.Duration
}

// NewSessions собирает менеджер сессий: store — хранилище #29, ttl —
// срок жизни сессии (DefaultSessionTTL при неположительном значении).
func NewSessions(st *store.Store, ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &Sessions{store: st, ttl: ttl}
}

// hashToken — SHA-256 токена в hex: то, что лежит в базе.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newToken — случайный токен сессии: 32 байта в base64-url.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// newCSRFToken — случайный CSRF-секрет сессии: 32 байта в base64-url.
func newCSRFToken() (string, error) { return newToken() }

// Start создаёт новую сессию пользователя и возвращает её токен и
// CSRF-секрет. Токен выдаётся один раз: дальше живёт только хэш.
func (s *Sessions) Start(ctx context.Context, userID string, now time.Time) (token, csrf string, err error) {
	token, err = newToken()
	if err != nil {
		return "", "", err
	}
	csrf, err = newCSRFToken()
	if err != nil {
		return "", "", err
	}
	data, err := json.Marshal(sessionData{CSRF: csrf})
	if err != nil {
		return "", "", fmt.Errorf("encode session data: %w", err)
	}
	expires := now.Add(s.ttl)
	if err := s.store.CreateSession(ctx, store.Session{
		TokenHash: hashToken(token),
		UserID:    userID,
		ExpiresAt: &expires,
		Data:      data,
	}); err != nil {
		return "", "", fmt.Errorf("create session: %w", err)
	}
	return token, csrf, nil
}

// Load находит действующую сессию по токену: истёкшая сессия удаляется
// лениво и считается отсутствующей. Возвращает ID пользователя и
// CSRF-секрет сессии.
func (s *Sessions) Load(ctx context.Context, token string, now time.Time) (userID, csrf string, err error) {
	sess, err := s.store.GetSession(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return "", "", ErrSessionNotFound
	}
	if err != nil {
		return "", "", err
	}
	if sess.ExpiresAt != nil && now.After(*sess.ExpiresAt) {
		// Ленивая уборка: срок вышел — сессии на сервере больше нет.
		_ = s.store.DeleteSession(ctx, sess.TokenHash)
		return "", "", ErrSessionNotFound
	}
	var data sessionData
	if len(sess.Data) > 0 {
		if err := json.Unmarshal(sess.Data, &data); err != nil {
			return "", "", fmt.Errorf("decode session data: %w", err)
		}
	}
	return sess.UserID, data.CSRF, nil
}

// Revoke удаляет сессию по токену (logout). Отсутствующая сессия —
// не ошибка: отзыв идемпотентен.
func (s *Sessions) Revoke(ctx context.Context, token string) error {
	err := s.store.DeleteSession(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// RevokeAllOfUser отзывает все сессии пользователя (блокировка, сброс
// пароля) и возвращает их число.
func (s *Sessions) RevokeAllOfUser(ctx context.Context, userID string) (int64, error) {
	return s.store.DeleteSessionsOfUser(ctx, userID)
}

// Cleanup удаляет истёкшие сессии из базы; возвращает число удалённых.
func (s *Sessions) Cleanup(ctx context.Context, now time.Time) (int64, error) {
	return s.store.DeleteExpiredSessions(ctx, now)
}
