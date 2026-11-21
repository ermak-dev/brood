package store

import (
	_ "embed"
	"errors"
)

//go:embed coordination.sql
var coordinationSchema string

// ErrCoordinationConflict identifies an occupied workspace slot or a stale
// operation identity.
var ErrCoordinationConflict = errors.New("coordination conflict")

// CoordinationConflictError is a stable domain conflict, not a SQLite constraint error.
type CoordinationConflictError struct{ Reason string }

func (e *CoordinationConflictError) Error() string { return "coordination conflict: " + e.Reason }
func (e *CoordinationConflictError) Unwrap() error { return ErrCoordinationConflict }
func newCoordinationConflict(reason string) error  { return &CoordinationConflictError{Reason: reason} }

// Регистрация таблицы в общей схеме — schemaModules ("coordination")
// и verifyCoordinationSchema в registry.go (#205).
