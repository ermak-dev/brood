// Тесты пользователей: CRUD, уникальность username, смена флагов и пароля.
package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// Конкурирующие изменения на двух соединениях не оставляют базу без админа.
func TestLastAdminConcurrentMutations(t *testing.T) {
	mutations := []struct {
		name string
		run  func(*Store, context.Context, string) error
	}{
		{"disable", func(s *Store, ctx context.Context, id string) error { return s.SetUserDisabled(ctx, id, true) }},
		{"demote", func(s *Store, ctx context.Context, id string) error { return s.SetUserAdmin(ctx, id, false) }},
		{"delete", func(s *Store, ctx context.Context, id string) error { return s.DeleteUser(ctx, id) }},
	}
	for _, a := range mutations {
		for _, b := range mutations {
			t.Run(a.name+"/"+b.name, func(t *testing.T) {
				ctx := t.Context()
				path := filepath.Join(t.TempDir(), "race.sqlite3")
				s1, err := Open(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				defer s1.Close()
				s2, err := Open(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				defer s2.Close()
				for _, id := range []string{"a", "b"} {
					if err := s1.CreateUser(ctx, User{ID: id, Username: id, PasswordHash: "h", IsAdmin: true}); err != nil {
						t.Fatal(err)
					}
					if err := s1.CreateSession(ctx, Session{TokenHash: id, UserID: id}); err != nil {
						t.Fatal(err)
					}
				}
				start := make(chan struct{})
				results := make(chan error, 2)
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); <-start; results <- a.run(s1, ctx, "a") }()
				go func() { defer wg.Done(); <-start; results <- b.run(s2, ctx, "b") }()
				close(start)
				wg.Wait()
				close(results)
				success := 0
				for err := range results {
					if err == nil {
						success++
					} else if !errors.Is(err, ErrLastActiveAdmin) {
						t.Errorf("unexpected mutation failure: %v", err)
					}
				}
				if success != 1 {
					t.Errorf("successful mutations=%d, want 1", success)
				}
				if n, err := s1.CountActiveAdmins(ctx); err != nil || n != 1 {
					t.Fatalf("active admins=%d err=%v", n, err)
				}
				for _, id := range []string{"a", "b"} {
					u, err := s1.GetUser(ctx, id)
					_, serr := s1.GetSession(ctx, id)
					if err == nil && u.IsAdmin && !u.Disabled {
						if serr != nil {
							t.Errorf("survivor session: %v", serr)
						}
					} else if !errors.Is(serr, ErrNotFound) {
						t.Errorf("revoked user %s retained session: %v", id, serr)
					}
				}
			})
		}
	}
}

func TestSessionCreationCannotOutliveConcurrentDisable(t *testing.T) {
	for i := 0; i < 10; i++ {
		s := newTestStore(t)
		ctx := t.Context()
		seedUser(t, s, "u", "u")
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() { <-start; results <- s.CreateSession(ctx, Session{TokenHash: "token", UserID: "u"}) }()
		go func() { <-start; results <- s.SetUserDisabled(ctx, "u", true) }()
		close(start)
		<-results
		<-results
		if _, err := s.GetSession(ctx, "token"); !errors.Is(err, ErrNotFound) {
			t.Errorf("session survived concurrent disable: %v", err)
		}
		if err := s.CreateSession(ctx, Session{TokenHash: "late", UserID: "u"}); err == nil {
			t.Error("created session for disabled user")
		}
	}
}

func TestUserSecurityMutationRollback(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seedUser(t, s, "u", "u")
	if err := s.CreateSession(ctx, Session{TokenHash: "token", UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort")
	if err := s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.SetUserDisabled(ctx, "u", true); err != nil {
			return err
		}
		if _, err := s.GetSession(ctx, "token"); !errors.Is(err, ErrNotFound) {
			t.Errorf("session not revoked inside transaction: %v", err)
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if u, err := s.GetUser(ctx, "u"); err != nil || u.Disabled {
		t.Fatalf("rollback user: %+v %v", u, err)
	}
	if _, err := s.GetSession(ctx, "token"); err != nil {
		t.Fatalf("rollback session: %v", err)
	}
}

func TestCreateGetUserByUsername(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u := User{
		ID:           "01J5Z5Z5Z5Z5Z5Z5Z5Z5Z5Z5Z5",
		Username:     "alice",
		DisplayName:  ptr("Alice"),
		PasswordHash: "argon2id$hash",
		IsAdmin:      true,
	}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	got, err := s.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("ID = %q, want %q", got.ID, u.ID)
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want alice", got.Username)
	}
	if got.DisplayName == nil || *got.DisplayName != "Alice" {
		t.Errorf("DisplayName = %v, want pointer to Alice", got.DisplayName)
	}
	if got.PasswordHash != "argon2id$hash" {
		t.Errorf("PasswordHash = %q", got.PasswordHash)
	}
	if !got.IsAdmin {
		t.Error("IsAdmin = false, want true")
	}
	if got.Disabled {
		t.Error("Disabled = true, want false")
	}
	if got.CreatedAt == nil || got.UpdatedAt == nil {
		t.Error("CreatedAt/UpdatedAt not set by store")
	}
}

// TestCreateUserRequiresUniqueUsername проверяет отказ при дублировании username.
func TestCreateUserRequiresUniqueUsername(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u := User{ID: "u1", Username: "dup", PasswordHash: "h"}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	err := s.CreateUser(ctx, User{ID: "u2", Username: "dup", PasswordHash: "h"})
	if err == nil {
		t.Fatal("second CreateUser with same username succeeded, want error")
	}
	if !IsUniqueViolation(err) {
		t.Errorf("error is not unique violation: %v", err)
	}
}

// TestGetUser проверяет чтение пользователя по ID.
func TestGetUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateUser(ctx, User{ID: "u1", Username: "alice", PasswordHash: "h"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	got, err := s.GetUser(ctx, "u1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want alice", got.Username)
	}
}

// TestGetUserMissing проверяет ErrNotFound для отсутствующего пользователя.
func TestGetUserMissing(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetUser(context.Background(), "nope")
	if !isNotFound(err) {
		t.Errorf("GetUser missing = %v, want ErrNotFound", err)
	}
	_, err = s.GetUserByUsername(context.Background(), "nope")
	if !isNotFound(err) {
		t.Errorf("GetUserByUsername missing = %v, want ErrNotFound", err)
	}
}

// TestListUsers проверяет перечисление пользователей.
func TestListUsers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i, name := range []string{"alice", "bob"} {
		if err := s.CreateUser(ctx, User{
			ID:           string(rune('a' + i)),
			Username:     name,
			PasswordHash: "h",
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("len(ListUsers) = %d, want 2", len(users))
	}
	if users[0].Username != "alice" || users[1].Username != "bob" {
		t.Errorf("users not ordered by username: %s, %s", users[0].Username, users[1].Username)
	}
}

// TestSetUserFields проверяет SetPasswordHash, SetUserDisabled, SetUserAdmin.
func TestSetUserFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateUser(ctx, User{ID: "u1", Username: "alice", PasswordHash: "h"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.SetPasswordHash(ctx, "u1", "newhash"); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}
	// Блокируем до выдачи прав: охранник последнего администратора
	// не должен мешать блокировке обычного пользователя.
	if err := s.SetUserDisabled(ctx, "u1", true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	if err := s.SetUserAdmin(ctx, "u1", true); err != nil {
		t.Fatalf("SetUserAdmin: %v", err)
	}
	got, err := s.GetUser(ctx, "u1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.PasswordHash != "newhash" {
		t.Errorf("PasswordHash = %q, want newhash", got.PasswordHash)
	}
	if !got.IsAdmin {
		t.Error("IsAdmin = false, want true")
	}
	if !got.Disabled {
		t.Error("Disabled = false, want true")
	}

	if err := s.SetPasswordHash(ctx, "nope", "x"); !isNotFound(err) {
		t.Errorf("SetPasswordHash missing = %v, want ErrNotFound", err)
	}
	if err := s.SetUserAdmin(ctx, "nope", true); !isNotFound(err) {
		t.Errorf("SetUserAdmin missing = %v, want ErrNotFound", err)
	}
	if err := s.SetUserDisabled(ctx, "nope", true); !isNotFound(err) {
		t.Errorf("SetUserDisabled missing = %v, want ErrNotFound", err)
	}
}

// TestCountActiveAdmins и TestBootstrapAdmin: хелпер для первого
// администратора идемпотентен и считает только активных админов.
func TestCountActiveAdmins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if n, err := s.CountActiveAdmins(ctx); err != nil || n != 0 {
		t.Fatalf("CountActiveAdmins on empty = %d, %v, want 0", n, err)
	}
	if err := s.CreateUser(ctx, User{ID: "a1", Username: "admin", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatalf("CreateUser admin: %v", err)
	}
	if n, err := s.CountActiveAdmins(ctx); err != nil || n != 1 {
		t.Fatalf("CountActiveAdmins = %d, %v, want 1", n, err)
	}
	// Единственного активного администратора блокировать нельзя.
	if err := s.SetUserDisabled(ctx, "a1", true); err == nil {
		t.Fatal("disabling the only active admin succeeded, want error")
	}
	// Второй администратор снимает охрану.
	if err := s.CreateUser(ctx, User{ID: "a2", Username: "admin2", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatalf("CreateUser admin2: %v", err)
	}
	if err := s.SetUserDisabled(ctx, "a1", true); err != nil {
		t.Fatalf("SetUserDisabled with second admin: %v", err)
	}
	if n, err := s.CountActiveAdmins(ctx); err != nil || n != 1 {
		t.Fatalf("CountActiveAdmins with disabled admin = %d, %v, want 1", n, err)
	}
	if err := s.SetUserDisabled(ctx, "a2", true); err == nil {
		t.Fatal("disabling the last active admin succeeded, want error")
	}
}

func TestBootstrapAdmin(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created, err := s.BootstrapAdmin(ctx, "admin", "hash1")
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	if !created {
		t.Error("created = false on first call, want true")
	}
	got, err := s.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if !got.IsAdmin {
		t.Error("bootstrapped user is not admin")
	}
	if got.Disabled {
		t.Error("bootstrapped user is disabled")
	}
	if got.PasswordHash != "hash1" {
		t.Errorf("PasswordHash = %q, want hash1", got.PasswordHash)
	}

	created, err = s.BootstrapAdmin(ctx, "admin", "hash2")
	if err != nil {
		t.Fatalf("second BootstrapAdmin: %v", err)
	}
	if created {
		t.Error("created = true on second call, want false")
	}
	got, err = s.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername after second call: %v", err)
	}
	if got.PasswordHash != "hash1" {
		t.Errorf("PasswordHash changed by no-op bootstrap: %q", got.PasswordHash)
	}
}

// TestEnsureAdminGuardTwoAdmins проверяет, что охранник последнего
// администратора считает всех активных админов.
func TestEnsureAdminGuardTwoAdmins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i, name := range []string{"admin1", "admin2"} {
		if err := s.CreateUser(ctx, User{
			ID:           string(rune('a' + i)),
			Username:     name,
			PasswordHash: "h",
			IsAdmin:      true,
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
	}
	err := s.SetUserDisabled(ctx, "a", true)
	if err != nil {
		t.Errorf("SetUserDisabled of one of two admins = %v, want nil", err)
	}
	err = s.SetUserDisabled(ctx, "b", true)
	if err == nil {
		t.Fatal("SetUserDisabled of last admin succeeded, want error")
	}
}

// TestUserDisplayNameNullable проверяет чтение NULL display_name.
func TestUserDisplayNameNullable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateUser(ctx, User{ID: "u1", Username: "alice", PasswordHash: "h"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	got, err := s.GetUser(ctx, "u1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.DisplayName != nil {
		t.Errorf("DisplayName = %v, want nil", got.DisplayName)
	}
}
