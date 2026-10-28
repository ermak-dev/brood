package app

import "errors"

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
	// ErrInvalidRole refuses role names outside the local ACL dictionary.
	ErrInvalidRole = errors.New("swarm role must be one of: owner, editor, viewer")
)
