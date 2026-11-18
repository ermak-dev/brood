package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

// ErrDurableTransactionNested refuses effect permission before an outer commit.
var ErrDurableTransactionNested = errors.New("durable effect transaction cannot run inside another transaction")

// WithDurableTx commits effect reservations with synchronous=FULL on a pinned WAL
// connection before returning. Ordinary transactions keep their existing setting.
// The callback must use its transaction context for all Store operations.
// External effects must start only after a nil return, never inside the callback.
func (s *Store) WithDurableTx(ctx context.Context, fn func(context.Context) error) (result error) {
	if _, nested := ctx.Value(txKey{}).(*sql.Tx); nested {
		return ErrDurableTransactionNested
	}
	if fn == nil {
		return errors.New("durable effect transaction requires a callback")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open durable effect connection: %w", err)
	}
	defer conn.Close()
	var journal string
	var previous int
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		return err
	}
	if journal != "wal" {
		return errors.New("durable effect transaction requires WAL storage")
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&previous); err != nil {
		return err
	}
	if previous < 0 || previous > 3 {
		return errors.New("invalid database durability setting")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, fmt.Sprintf("PRAGMA synchronous=%d", previous)); err != nil {
			// Не возвращаем в пул соединение с неизвестным состоянием pragma.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			result = errors.Join(result, fmt.Errorf("restore database durability setting: %w", err))
		}
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA synchronous=FULL`); err != nil {
		return fmt.Errorf("enable durable effect commit: %w", err)
	}
	var mode int
	if err := conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode); err != nil {
		return err
	}
	if mode != 2 {
		return errors.New("database did not enable durable effect commits")
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin durable effect transaction: %w", err)
	}
	defer tx.Rollback()
	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit durable effect transaction: %w", err)
	}
	return nil
}
