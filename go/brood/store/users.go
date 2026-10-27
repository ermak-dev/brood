// Пользователи: CRUD, смена пароля и флагов, bootstrap администратора.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// User — строка таблицы users. Timestamp — UTC RFC3339, id задаёт
// вызывающий (ULID/UUID прикладного слоя).
type User struct {
	ID           string
	Username     string
	DisplayName  *string
	PasswordHash string
	IsAdmin      bool
	Disabled     bool
	CreatedAt    *string
	UpdatedAt    *string
}

// scanUser читает строку users в фиксированном порядке колонок.
func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var createdAt, updatedAt string
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash,
		&u.IsAdmin, &u.Disabled, &createdAt, &updatedAt)
	if err != nil {
		return u, err
	}
	u.CreatedAt = &createdAt
	u.UpdatedAt = &updatedAt
	return u, nil
}

// userCols — список колонок users для SELECT.
const userCols = `id, username, display_name, password_hash, is_admin, disabled, created_at, updated_at`

// CreateUser вставляет пользователя; store проставляет created_at и
// updated_at. Дубликат username — ошибка уникальности (IsUniqueViolation).
func (s *Store) CreateUser(ctx context.Context, u User) error {
	_, err := s.conn(ctx).ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, is_admin, disabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.DisplayName, u.PasswordHash, u.IsAdmin, u.Disabled,
		nowRFC3339(), nowRFC3339())
	if err != nil {
		return wrapUnique(err, fmt.Sprintf("user %q", u.Username))
	}
	return nil
}

// GetUser возвращает пользователя по ID.
func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	u, err := scanUser(s.conn(ctx).QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return u, fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	return u, err
}

// GetUserByUsername возвращает пользователя по имени входа.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	u, err := scanUser(s.conn(ctx).QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE username = ?`, username))
	if err == sql.ErrNoRows {
		return u, fmt.Errorf("%w: user %q", ErrNotFound, username)
	}
	return u, err
}

// ListUsers возвращает всех пользователей по имени.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.conn(ctx).QueryContext(ctx,
		`SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("list users: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// SetPasswordHash заменяет хэш пароля пользователя.
func (s *Store) SetPasswordHash(ctx context.Context, id, passwordHash string) error {
	return s.updateUser(ctx, id, `SET password_hash = ?, updated_at = ?`, passwordHash, nowRFC3339())
}

// ErrLastActiveAdmin запрещает удаление последнего способа управлять установкой.
var ErrLastActiveAdmin = errors.New("cannot remove the last active admin")

// SetUserAdmin меняет роль и атомарно отзывает сессии при понижении.
func (s *Store) SetUserAdmin(ctx context.Context, id string, admin bool) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if !admin {
			if err := s.guardActiveAdmin(ctx, id); err != nil {
				return err
			}
		}
		if err := s.updateUser(ctx, id, `SET is_admin = ?, updated_at = ?`, admin, nowRFC3339()); err != nil {
			return err
		}
		if !admin {
			_, err := s.DeleteSessionsOfUser(ctx, id)
			return err
		}
		return nil
	})
}

// SetUserDisabled блокирует пользователя и отзывает сессии в одной
// транзакции; последнего активного администратора блокировать нельзя.
func (s *Store) SetUserDisabled(ctx context.Context, id string, disabled bool) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if disabled {
			if err := s.guardActiveAdmin(ctx, id); err != nil {
				return err
			}
		}
		if err := s.updateUser(ctx, id, `SET disabled = ?, updated_at = ?`, disabled, nowRFC3339()); err != nil {
			return err
		}
		if disabled {
			_, err := s.DeleteSessionsOfUser(ctx, id)
			return err
		}
		return nil
	})
}

// Вызывается под блокировкой записи WithTx: проверка и изменение
// сериализованы даже между разными соединениями одной базы.
func (s *Store) guardActiveAdmin(ctx context.Context, id string) error {
	u, err := s.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if !u.IsAdmin || u.Disabled {
		return nil
	}
	n, err := s.CountActiveAdmins(ctx)
	if err != nil {
		return err
	}
	if n < 2 {
		return ErrLastActiveAdmin
	}
	return nil
}

// updateUser выполняет UPDATE users с данным фрагментом SET.
func (s *Store) updateUser(ctx context.Context, id, set string, args ...any) error {
	res, err := s.conn(ctx).ExecContext(ctx,
		`UPDATE users `+set+` WHERE id = ?`, append(args, id)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	return nil
}

// DeleteUser физически удаляет пользователя; каскад убирает его
// членства, рои и сессии.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.guardActiveAdmin(ctx, id); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
		return err
	})
}

// CountActiveAdmins возвращает число не заблокированных администраторов.
// Экспортирован для охраны последнего администратора в прикладном слое.
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.conn(ctx).QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE is_admin = 1 AND disabled = 0`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active admins: %w", err)
	}
	return n, nil
}

// BootstrapAdmin идемпотентно заводит первого администратора. Если
// пользователь с данным username уже есть, ничего не меняет и возвращает
// created=false; пароль передаётся уже хэшированным (Argon2id), команда
// сервера отвечает за интерактивный ввод без эха. Администратора нет —
// создаётся активный пользователь с правами администратора.
func (s *Store) BootstrapAdmin(ctx context.Context, username, passwordHash string) (created bool, err error) {
	err = s.WithTx(ctx, func(ctx context.Context) error {
		var exists int
		err := s.conn(ctx).QueryRowContext(ctx,
			`SELECT 1 FROM users WHERE username = ?`, username).Scan(&exists)
		if err == nil {
			return nil // уже есть: ничего не трогаем
		}
		if err != sql.ErrNoRows {
			return err
		}
		created = true
		return s.CreateUser(ctx, User{
			ID:           NewID(),
			Username:     username,
			PasswordHash: passwordHash,
			IsAdmin:      true,
		})
	})
	if err != nil {
		return false, fmt.Errorf("bootstrap admin: %w", err)
	}
	return created, nil
}
