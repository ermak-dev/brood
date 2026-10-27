// Общие помощники тестов пакета store.
package store

import (
	"context"
	"errors"
	"testing"
)

// newTestStore открывает хранилище во временном каталоге теста.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), t.TempDir()+"/db.sqlite3")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ptr — адрес значения, для полей-указателей в тестах.
func ptr[T any](v T) *T { return &v }

// isNotFound сокращает проверку ErrNotFound с обёрткой.
func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// seedUser заводит пользователя, если его ещё нет.
func seedUser(t *testing.T, s *Store, id, username string) {
	t.Helper()
	_, err := s.GetUser(context.Background(), id)
	if err == nil {
		return
	}
	if !isNotFound(err) {
		t.Fatalf("seed user %s: %v", id, err)
	}
	err = s.CreateUser(context.Background(), User{ID: id, Username: username, PasswordHash: "h"})
	if err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// seedProject заводит проект, если его ещё нет.
func seedProject(t *testing.T, s *Store, id, name string) {
	t.Helper()
	_, err := s.GetProject(context.Background(), id)
	if err == nil {
		return
	}
	if !isNotFound(err) {
		t.Fatalf("seed project %s: %v", id, err)
	}
	err = s.CreateProject(context.Background(), Project{ID: id, Name: name})
	if err != nil {
		t.Fatalf("seed project %s: %v", id, err)
	}
}

// seedProjectWithMember заводит проект, пользователя и членство.
func seedProjectWithMember(t *testing.T, s *Store, projectID, userID string) {
	t.Helper()
	seedProject(t, s, projectID, projectID+"-name")
	seedUser(t, s, userID, userID+"-user")
	if err := s.AddMember(context.Background(), projectID, userID); err != nil {
		t.Fatalf("seed member %s/%s: %v", projectID, userID, err)
	}
}
