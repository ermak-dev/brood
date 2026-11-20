package app

import (
	"errors"
	"fmt"
)

// Ошибки прикладного слоя: строкой с причиной, без паник и трассировок.
// HTTP-слой отображает их статусами, CLI — текстом.
var (
	// ErrNotAllowed — операция запрещена правилами доступа.
	ErrNotAllowed = errors.New("not allowed")
	// ErrNotFound — объект не существует или недоступен вызывающему.
	ErrNotFound = errors.New("not found")
	// ErrDisabled — пользователь заблокирован.
	ErrDisabled = errors.New("user disabled")
	// ErrExists — объект с такими ключами уже есть.
	ErrExists = errors.New("already exists")
	// ErrConflict — конфликт состояния, например чужой atespace с тем же
	// именем.
	ErrConflict = errors.New("conflict")
	// ErrState — переход состояния роя нелегален.
	ErrState = errors.New("illegal state transition")
	// ErrSwarmNotEmpty identifies a confirmed atomic refusal without native deletion.
	ErrSwarmNotEmpty = errors.New("swarm is not empty")
	// ErrInvalidWorkload rejects unresolved resources or placement outside the shard.
	ErrInvalidWorkload = errors.New("invalid workload admission")
	// ErrInvalidCapacity rejects unsupported shard limits or worker labels.
	ErrInvalidCapacity = errors.New("invalid shard capacity")
)

// APIError — отказ, готовый к HTTP-ответу: статус и причина строкой.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Detail) }
