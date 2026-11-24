package app

import (
	"errors"
	"fmt"
)

// Ошибки прикладного слоя: строкой с причиной, без паник и трассировок.
// HTTP-слой отображает их статусами, команды — текстом.
var (
	// ErrNotAllowed refuses an operation the access rules do not permit.
	ErrNotAllowed = errors.New("not allowed")
	// ErrNotFound reports an object that does not exist or is hidden from the caller.
	ErrNotFound = errors.New("not found")
	// ErrExists reports an object with the same key.
	ErrExists = errors.New("already exists")
	// ErrConflict reports a state that changed or does not permit the operation.
	ErrConflict = errors.New("conflict")
	// ErrInvalidWorkload rejects an agent without a name or resolved CPU and memory.
	ErrInvalidWorkload = errors.New("invalid workload admission")
	// ErrInvalidCapacity rejects malformed project limits or pool budgets.
	ErrInvalidCapacity = errors.New("invalid capacity")
)

// APIError is a refusal ready for an HTTP response: a status and the reason.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Detail) }
