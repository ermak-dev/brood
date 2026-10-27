// Package store — локальное хранилище сервера brood: SQLite (WAL),
// embedded-миграции и явные запросы без ORM. Пакет молчит: возвращает
// данные и ошибки, печатает только вызывающий командлет.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"time"
)

// ErrNotFound возвращает вызывающему, когда записи с данным ключом нет.
var ErrNotFound = fmt.Errorf("record not found")

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrations — имена встроенных миграций в порядке применения.
var migrations = func() []string {
	entries, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		panic(fmt.Sprintf("store: embedded migrations: %v", err))
	}
	sort.Strings(entries)
	return entries
}()

// Store — обёртка над database/sql с методами домена. Безопасна для
// конкурентного использования: единственное соединение SQLite (WAL,
// busy_timeout) сериализует транзакции без отказов SQLITE_BUSY.
type Store struct {
	db *sql.DB
}

// dsn собирает строку подключения с прагмами на каждое соединение:
// WAL, внешние ключи и ожидание блокировки вместо отказа.
func dsn(path string) string {
	return "file:" + url.PathEscape(path) +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=synchronous(NORMAL)"
}

// Open открывает (или создаёт) базу по пути и идемпотентно применяет
// миграции.
func Open(ctx context.Context, path string) (*Store, error) {
	if path != ":memory:" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("create private database: %w", err)
		}
		if err == nil {
			if err := file.Close(); err != nil {
				return nil, err
			}
		}
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// Единственное физическое соединение: SQLite не даёт параллельных
	// писателей, а busy_timeout внутри соединения ждёт блокировку.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	// Ping материализует соединение: ошибки прагм DSN (например,
	// journal_mode при чужой блокировке записи) выходят здесь, где их
	// можно повторить, а не в первой случайной операции.
	err = retryOnBusy(func() error {
		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("open database %s: %w", path, err)
		}
		return s.applyMigrations(ctx)
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close закрывает соединение с базой.
func (s *Store) Close() error { return s.db.Close() }

// applyMigrations применяет необработанные миграции; выполненные уже
// записаны в schema_migrations и пропускаются, поэтому повторное
// открытие базы безопасно. Вся работа — в транзакции на блокировке
// записи (BEGIN IMMEDIATE): конкурентные Open одной базы сериализуются
// через busy_timeout, а не падают от взаимоблокировки.
func (s *Store) applyMigrations(ctx context.Context) error {
	for _, name := range migrations {
		if err := retryOnBusy(func() error {
			return s.applyMigration(ctx, name)
		}); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration применяет одну миграцию в транзакции на блокировке
// записи, если она ещё не применена.
func (s *Store) applyMigration(ctx context.Context, name string) error {
	body, err := migrationsFS.ReadFile(name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", name, err)
	}
	tx, err := s.db.BeginTx(ctx, nil) // _txlock=immediate
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", name, err)
	}
	if err := func() error {
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS schema_migrations (
				name TEXT PRIMARY KEY,
				applied_at TEXT NOT NULL
			)`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		var exists int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE name = ?`, name).Scan(&exists)
		if err == sql.ErrNoRows {
			exists = 0
		} else if err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists == 1 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			name, nowRFC3339()); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		return nil
	}(); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	return nil
}

// DB — то, на чём выполняются запросы: пул соединений или транзакция.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// txKey — ключ контекста для активной транзакции.
type txKey struct{}

// retryOnBusy повторяет fn при SQLITE_BUSY: первый запрос нового
// физического соединения (ping) проходит прагмы соединения до
// установки busy_timeout и получает отказ без ожидания (например,
// journal_mode при чужой блокировке записи), поэтому миграции ждут
// и повторяют попытку сами.
func retryOnBusy(fn func() error) error {
	var err error
	for try := 0; try < 100; try++ {
		err = markBusy(fn())
		if !errors.Is(err, errBusy) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("database still busy after retries: %w", err)
}

// conn возвращает executor запросов: транзакцию из ctx или пул.
func (s *Store) conn(ctx context.Context) DB {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok && tx != nil {
		return tx
	}
	return s.db
}

// WithTx выполняет fn в одной транзакции: фиксация при nil, откат при
// ошибке. Методы Store внутри fn берут транзакцию из ctx. Паника fn
// откатывает транзакцию и пробрасывается наверх. Вложенный вызов не
// создаёт вторую транзакцию, а продолжает текущую.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return fn(ctx)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	txCtx := context.WithValue(ctx, txKey{}, tx)
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()
	if err := fn(txCtx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return nil
}
