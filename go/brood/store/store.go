// Package store — локальное хранилище сервера brood: SQLite (WAL),
// встроенная текущая схема и явные запросы без ORM. Пакет молчит: возвращает
// данные и ошибки, печатает только вызывающий командлет.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrNotFound возвращает вызывающему, когда записи с данным ключом нет.
var ErrNotFound = fmt.Errorf("record not found")

// errIncompatibleSchema — общая причина отказов несовместимой схемы:
// ошибки заворачиваются в неё, вызывающие проверяют errors.Is.
var errIncompatibleSchema = errors.New("incompatible database schema")

// schemaVersion — единственная версия схемы базы. Миграций нет: база
// создаётся заново, любая другая версия отвергается (#205).
// 17: swarm_role_bindings (#208).
// 18: swarm_requests и их триггеры (#209), у swarm_steps нет колонки answer.
// DDL несовместим с 17: база версии 17 открывается кодом 18 только после
// пересоздания, поэтому версия растёт, а не остаётся.
// 19: нет pipeline_preparations, её триггеров и колонок native_* у
// swarm_steps (#223): модель «актор под узел» закрыта в #208.
// 20: состояние доставки ответа актору swarm_requests.delivery (#210).
// 21: project_git_credentials и project_git_credential_refs (#173).
const schemaVersion = 21

//go:embed schema.sql
var currentSchema string

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

// Open открывает текущую базу или создаёт новую по пути.
// Несовместимая схема отвергается без изменения таблиц.
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
		return s.initializeSchema(ctx)
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close закрывает соединение с базой.
func (s *Store) Close() error { return s.db.Close() }

// initializeSchema применяет единую схему. Пустая база (версия 0 без
// объектов) создаётся целиком и получает schemaVersion; совпадающая
// версия проверяется; любая другая — отказ без мутации файла. Проверка
// и создание держат BEGIN IMMEDIATE: конкурентные Open не видят частичную
// схему. Модули схемы объявляют только свой DDL и проверку (#205).
func (s *Store) initializeSchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema initialization: %w", err)
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("inspect schema version: %w", err)
	}
	var objects int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&objects); err != nil {
		return fmt.Errorf("inspect database schema: %w", err)
	}
	switch {
	case version == 0 && objects == 0:
		for _, module := range schemaModules {
			if _, err := tx.ExecContext(ctx, module.create()); err != nil {
				return fmt.Errorf("initialize database schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, schemaVersion)); err != nil {
			return fmt.Errorf("initialize schema version: %w", err)
		}
	case version == schemaVersion:
		for _, module := range schemaModules {
			if err := module.verify(ctx, tx); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: found %d, want %d; recreate the database", errIncompatibleSchema, version, schemaVersion)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema initialization: %w", err)
	}
	return nil
}

// checkSchema сверяет определения таблиц, включая ограничения. Пробелы
// несущественны. Посторонние служебные таблицы не меняются.
func checkSchema(ctx context.Context, tx *sql.Tx, expected string) error {
	for _, statement := range strings.Split(expected, ";") {
		fields := strings.Fields(statement)
		if len(fields) == 0 {
			continue
		}
		table := fields[2]
		var actual string
		err := tx.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&actual)
		if err == sql.ErrNoRows {
			return fmt.Errorf("incompatible database schema: missing table %s; recreate the database", table)
		}
		if err != nil {
			return fmt.Errorf("inspect table %s: %w", table, err)
		}
		if normalizeSchemaSQL(actual) != normalizeSchemaSQL(statement) {
			return fmt.Errorf("incompatible database schema: table %s differs from current schema; recreate the database", table)
		}
	}
	return nil
}

// normalizeSchemaSQL игнорирует пробелы только вне строковых литералов.
func normalizeSchemaSQL(statement string) string {
	parts := strings.Split(statement, "'")
	for i := 0; i < len(parts); i += 2 {
		parts[i] = strings.Join(strings.Fields(parts[i]), "")
	}
	return strings.Join(parts, "'")
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
// journal_mode при чужой блокировке записи), поэтому инициализация ждёт
// и повторяет попытку сама.
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
