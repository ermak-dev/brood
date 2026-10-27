package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"brood.dev/go/brood/store"
)

// seedUser — пользователь в базе: сессии связаны внешним ключом.
func seedUser(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.CreateUser(context.Background(), store.User{
		ID: id, Username: "u-" + id, PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func TestSessionsStartLoad(t *testing.T) {
	st := testStore(t)
	seedUser(t, st, "user1")
	s := NewSessions(st, time.Hour)
	now := time.Now()

	token, csrf, err := s.Start(context.Background(), "user1", now)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if token == "" || csrf == "" || token == csrf {
		t.Fatalf("token/csrf must be non-empty and distinct: %q %q", token, csrf)
	}
	// Сам токен не хранится: в базе только его SHA-256.
	_, err = st.GetSession(context.Background(), token)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("raw token must not be a lookup key: %v", err)
	}
	uid, gotCSRF, err := s.Load(context.Background(), token, now)
	if err != nil || uid != "user1" || gotCSRF != csrf {
		t.Fatalf("load: uid=%q csrf ok=%v err=%v", uid, gotCSRF == csrf, err)
	}
}

func TestSessionsExpiredLazyDeleted(t *testing.T) {
	st := testStore(t)
	seedUser(t, st, "user1")
	seedUser(t, st, "user2")
	s := NewSessions(st, time.Hour)
	now := time.Now()
	token, _, err := s.Start(context.Background(), "user1", now)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// После срока — Load отказывает и удаляет строку из базы.
	if _, _, err := s.Load(context.Background(), token, now.Add(2*time.Hour)); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expired load: %v", err)
	}
	if _, err := st.GetSession(context.Background(), hashToken(token)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired session must be deleted: %v", err)
	}
	// Cleanup убирает остальные истёкшие строки.
	token2, _, _ := s.Start(context.Background(), "user1", now.Add(-2*time.Hour))
	if n, err := s.Cleanup(context.Background(), now); err != nil || n != 1 {
		t.Fatalf("cleanup: n=%d err=%v", n, err)
	}
	if _, err := st.GetSession(context.Background(), hashToken(token2)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cleanup must remove expired session")
	}
}

func TestSessionsRevoke(t *testing.T) {
	st := testStore(t)
	seedUser(t, st, "user1")
	seedUser(t, st, "user2")
	s := NewSessions(st, time.Hour)
	now := time.Now()
	token, _, _ := s.Start(context.Background(), "user1", now)
	if err := s.Revoke(context.Background(), token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Повторный отзыв молчит: logout идемпотентен.
	if err := s.Revoke(context.Background(), token); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
}

func TestSessionsRevokeAllOfUser(t *testing.T) {
	st := testStore(t)
	seedUser(t, st, "user1")
	seedUser(t, st, "user2")
	s := NewSessions(st, time.Hour)
	now := time.Now()
	for range 3 {
		if _, _, err := s.Start(context.Background(), "user1", now); err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	s.Start(context.Background(), "user2", now)
	n, err := s.RevokeAllOfUser(context.Background(), "user1")
	if err != nil || n != 3 {
		t.Fatalf("revoke all: n=%d err=%v", n, err)
	}
}

func TestSessionsUniqueTokens(t *testing.T) {
	st := testStore(t)
	seedUser(t, st, "user1")
	seedUser(t, st, "user2")
	s := NewSessions(st, time.Hour)
	now := time.Now()
	seen := make(map[string]bool)
	for range 64 {
		token, _, err := s.Start(context.Background(), "user1", now)
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if seen[token] {
			t.Fatal("session tokens must not repeat")
		}
		seen[token] = true
	}
}
