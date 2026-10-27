// Тесты транзакций и конкурентного доступа.
package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// TestWithTxCommit проверяет фиксацию транзакции.
func TestWithTxCommit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.WithTx(ctx, func(ctx context.Context) error {
		return s.CreateUser(ctx, User{ID: "u1", Username: "alice", PasswordHash: "h"})
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if _, err := s.GetUserByUsername(ctx, "alice"); err != nil {
		t.Fatalf("committed user not found: %v", err)
	}
}

// TestWithTxRollback проверяет откат транзакции при ошибке fn.
func TestWithTxRollback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	want := errors.New("boom")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.CreateUser(ctx, User{ID: "u1", Username: "alice", PasswordHash: "h"}); err != nil {
			return err
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("WithTx error = %v, want %v", err, want)
	}
	if _, err := s.GetUserByUsername(ctx, "alice"); !isNotFound(err) {
		t.Errorf("rolled back user visible: %v", err)
	}
}

// TestWithTxNestedPanic: fn, сломавшийся паникой, не оставляет открытую
// транзакцию.
func TestWithTxNestedPanic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	defer func() {
		if recover() == nil {
			t.Error("panic not propagated")
		}
		if _, err := s.GetUserByUsername(ctx, "alice"); !isNotFound(err) {
			t.Errorf("user committed after panic: %v", err)
		}
	}()
	_ = s.WithTx(ctx, func(ctx context.Context) error {
		_ = s.CreateUser(ctx, User{ID: "u1", Username: "alice", PasswordHash: "h"})
		panic("boom")
	})
}

// TestConcurrentWrite: конкурентные записи из нескольких горутин
// не приводят к отказу SQLITE_BUSY.
func TestConcurrentWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const workers = 4
	const per = 10

	errs := make(chan error, workers*per)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				id := string(rune('a'+w)) + string(rune('0'+i))
				err := s.CreateUser(ctx, User{
					ID: id, Username: "user-" + id, PasswordHash: "h",
				})
				if err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}

	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != workers*per {
		t.Errorf("users = %d, want %d", len(users), workers*per)
	}
}

// TestConcurrentOpen: повторное открытие базы применяет миграции без
// гонки на schema_migrations.
func TestConcurrentOpen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/db.sqlite3"

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := Open(context.Background(), path)
			if err != nil {
				errs <- err
				return
			}
			st.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent open: %v", err)
	}
}

// TestOpenErrorPath: отказ открытия несёт понятную причину.
func TestOpenErrorPath(t *testing.T) {
	// Каталог вместо файла базы.
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Open panicked: %v", r)
			}
		}()
		_, err = Open(context.Background(), t.TempDir())
		return
	}()
	if err == nil {
		t.Fatal("Open of directory succeeded, want error")
	}
	if !strings.Contains(err.Error(), "open database") {
		t.Errorf("error %q does not mention open database", err)
	}
}
