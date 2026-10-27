// Вспомогательные функции пакета store.
package store

import (
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// nowRFC3339 возвращает текущий момент UTC в RFC3339 — формат всех
// timestamp в базе.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// errBusy — приметка отказа SQLITE_BUSY, на которую смотрит retryOnBusy.
var errBusy = errors.New("database is busy")

// UniqueViolationError — нарушение уникальности с понятным сообщением;
// Unwrap даёт доступ к исходной ошибке SQLite.
type UniqueViolationError struct {
	Msg string
	Err error
}

func (e *UniqueViolationError) Error() string { return e.Msg }
func (e *UniqueViolationError) Unwrap() error { return e.Err }

// wrapUnique переписывает нарушение уникальности в понятную ошибку,
// сохраняя классификацию для IsUniqueViolation.
func wrapUnique(err error, what string) error {
	if !IsUniqueViolation(err) {
		return err
	}
	return &UniqueViolationError{Msg: what + " already exists", Err: err}
}

// IsUniqueViolation сообщает, что отказ вызван нарушением уникальности.
func IsUniqueViolation(err error) bool {
	var uerr *UniqueViolationError
	if errors.As(err, &uerr) {
		return true
	}
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	return serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE ||
		serr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

// IsForeignKeyViolation сообщает, что отказ вызван нарушением внешнего
// ключа.
func IsForeignKeyViolation(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	return serr.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
}

// markBusy добавляет к отказу SQLITE_BUSY приметку errBusy для retry.
func markBusy(err error) error {
	var serr *sqlite.Error
	if errors.As(err, &serr) && serr.Code() == sqlite3.SQLITE_BUSY {
		return fmt.Errorf("%w: %w", err, errBusy)
	}
	return err
}
