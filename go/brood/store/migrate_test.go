// Тесты миграций: применение на чистой базе и повторное открытие.
package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestOpenAppliesMigrations проверяет, что Open создаёт все пять таблиц
// первой версии на чистой базе.
func TestOpenAppliesMigrations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, table := range []string{"users", "projects", "project_members", "swarms", "sessions"} {
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
		if err != nil {
			t.Fatalf("query sqlite_master for %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s not created by migrations", table)
		}
	}
}

// TestOpenIdempotentOnReopen проверяет, что повторное открытие той же базы
// применяет миграции без ошибки и не дублирует их.
func TestOpenIdempotentOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite3")
	ctx := context.Background()

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { second.Close() })

	var version int
	err = second.db.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations`).Scan(&version)
	if err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("schema_migrations has %d rows, want %d", version, len(migrations))
	}
}

// TestMigrationsSorted проверяет порядок имён встроенных миграций.
func TestMigrationsSorted(t *testing.T) {
	if len(migrations) == 0 {
		t.Fatal("no embedded migrations")
	}
	for i := 1; i < len(migrations); i++ {
		if migrations[i-1] >= migrations[i] {
			t.Errorf("migrations not sorted: %s >= %s", migrations[i-1], migrations[i])
		}
	}
}
