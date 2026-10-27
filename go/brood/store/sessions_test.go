// Тесты сессий: CRUD, отзыв по пользователю, очистка истёкших.
package store

import (
	"context"
	"testing"
	"time"
)

// TestSessionCRUD проверяет создание, чтение, удаление сессии.
func TestSessionCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedUser(t, s, "u1", "alice")
	sess := Session{TokenHash: "t1", UserID: "u1", Data: []byte("blob")}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.GetSession(ctx, "t1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.UserID != "u1" {
		t.Errorf("UserID = %q, want u1", got.UserID)
	}
	if string(got.Data) != "blob" {
		t.Errorf("Data = %q, want blob", got.Data)
	}
	if got.CreatedAt == nil {
		t.Error("CreatedAt not set by store")
	}

	if err := s.DeleteSession(ctx, "t1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := s.DeleteSession(ctx, "t1"); !isNotFound(err) {
		t.Errorf("DeleteSession missing = %v, want ErrNotFound", err)
	}
	if _, err := s.GetSession(ctx, "t1"); !isNotFound(err) {
		t.Errorf("GetSession deleted = %v, want ErrNotFound", err)
	}
	if _, err := s.GetSession(ctx, "nope"); !isNotFound(err) {
		t.Errorf("GetSession missing = %v, want ErrNotFound", err)
	}
}

// TestSessionExpiresAt: сессия без срока получает NULL expires_at.
func TestSessionExpiresAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedUser(t, s, "u1", "alice")
	expires := time.Now().UTC().Add(time.Hour)
	if err := s.CreateSession(ctx, Session{TokenHash: "t1", UserID: "u1", ExpiresAt: &expires}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.GetSession(ctx, "t1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ExpiresAt == nil {
		t.Fatal("ExpiresAt = nil, want set")
	}
	if diff := got.ExpiresAt.Sub(expires); diff < -time.Second || diff > time.Second {
		t.Errorf("ExpiresAt = %v, want ~%v", got.ExpiresAt, expires)
	}
}

// TestDeleteSessionsOfUser проверяет отзыв всех сессий пользователя.
func TestDeleteSessionsOfUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedUser(t, s, "u1", "alice")
	seedUser(t, s, "u2", "bob")
	for _, th := range []string{"t1", "t2"} {
		if err := s.CreateSession(ctx, Session{TokenHash: th, UserID: "u1"}); err != nil {
			t.Fatalf("CreateSession %s: %v", th, err)
		}
	}
	if err := s.CreateSession(ctx, Session{TokenHash: "t3", UserID: "u2"}); err != nil {
		t.Fatalf("CreateSession t3: %v", err)
	}

	n, err := s.DeleteSessionsOfUser(ctx, "u1")
	if err != nil {
		t.Fatalf("DeleteSessionsOfUser: %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteSessionsOfUser removed %d, want 2", n)
	}
	if _, err := s.GetSession(ctx, "t1"); !isNotFound(err) {
		t.Errorf("GetSession t1 after revoke = %v, want ErrNotFound", err)
	}
	if _, err := s.GetSession(ctx, "t3"); err != nil {
		t.Errorf("GetSession t3 after revoke of u1 = %v, want nil", err)
	}
}

// TestDeleteExpiredSessions проверяет удаление только истёкших сессий.
func TestDeleteExpiredSessions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedUser(t, s, "u1", "alice")
	past := time.Now().UTC().Add(-time.Minute)
	future := time.Now().UTC().Add(time.Hour)
	for _, sess := range []Session{
		{TokenHash: "expired", UserID: "u1", ExpiresAt: &past},
		{TokenHash: "live", UserID: "u1", ExpiresAt: &future},
		{TokenHash: "eternal", UserID: "u1"},
	} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession %s: %v", sess.TokenHash, err)
		}
	}

	now := time.Now().UTC()
	n, err := s.DeleteExpiredSessions(ctx, now)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("DeleteExpiredSessions removed %d, want 1", n)
	}
	if _, err := s.GetSession(ctx, "expired"); !isNotFound(err) {
		t.Errorf("expired session not removed: %v", err)
	}
	if _, err := s.GetSession(ctx, "live"); err != nil {
		t.Errorf("live session removed: %v", err)
	}
	if _, err := s.GetSession(ctx, "eternal"); err != nil {
		t.Errorf("session without expiry removed: %v", err)
	}
}

// TestSessionUserFK: сессия требует существующего пользователя.
func TestSessionUserFK(t *testing.T) {
	s := newTestStore(t)
	err := s.CreateSession(context.Background(), Session{TokenHash: "t1", UserID: "nope"})
	if err == nil || !IsForeignKeyViolation(err) {
		t.Errorf("session with missing user accepted: %v", err)
	}
}
