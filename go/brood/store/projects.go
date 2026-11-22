// Проекты и членство в них.
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Project — строка таблицы projects.
type Project struct {
	ID        string
	Name      string
	CreatedAt *string
}

// CreateProject inserts a project.
func (s *Store) CreateProject(ctx context.Context, p Project) error {
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO projects (id, name, created_at) VALUES (?, ?, ?)`, p.ID, p.Name, nowRFC3339())
	return wrapUnique(err, fmt.Sprintf("project %q", p.Name))
}

// GetProject возвращает проект по ID.
func (s *Store) GetProject(ctx context.Context, id string) (Project, error) {
	var p Project
	var createdAt string
	err := s.conn(ctx).QueryRowContext(ctx,
		`SELECT id, name, created_at FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &createdAt)
	if err == sql.ErrNoRows {
		return p, fmt.Errorf("%w: project %q", ErrNotFound, id)
	}
	if err != nil {
		return p, err
	}
	p.CreatedAt = &createdAt
	return p, nil
}

// DeleteProject удаляет проект; каскад убирает членства и рои проекта.
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	res, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: project %q", ErrNotFound, id)
	}
	return nil
}

// AddMember добавляет пользователя в проект. Отсутствующие проект или
// пользователь — нарушение внешнего ключа (IsForeignKeyViolation),
// повторное добавление — нарушение уникальности составного PK.
func (s *Store) AddMember(ctx context.Context, projectID, userID string) error {
	_, err := s.conn(ctx).ExecContext(ctx,
		`INSERT INTO project_members (project_id, user_id) VALUES (?, ?)`,
		projectID, userID)
	if err != nil {
		return wrapUnique(err, fmt.Sprintf("membership %s/%s", projectID, userID))
	}
	return nil
}

// RemoveMember исключает пользователя из проекта. Рои пользователя не
// удаляются: исключение отзывает доступ, но не уничтожает данные.
func (s *Store) RemoveMember(ctx context.Context, projectID, userID string) error {
	res, err := s.conn(ctx).ExecContext(ctx,
		`DELETE FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: membership %s/%s", ErrNotFound, projectID, userID)
	}
	return nil
}

// MembersOf возвращает участников проекта по имени.
func (s *Store) MembersOf(ctx context.Context, projectID string) ([]User, error) {
	rows, err := s.conn(ctx).QueryContext(ctx,
		`SELECT `+userCols+` FROM users u
		 JOIN project_members m ON m.user_id = u.id
		 WHERE m.project_id = ? ORDER BY u.username`, projectID)
	if err != nil {
		return nil, fmt.Errorf("members of %s: %w", projectID, err)
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("members of %s: %w", projectID, err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// IsMember сообщает, состоит ли пользователь в проекте.
func (s *Store) IsMember(ctx context.Context, projectID, userID string) (bool, error) {
	var exists int
	err := s.conn(ctx).QueryRowContext(ctx,
		`SELECT 1 FROM project_members WHERE project_id = ? AND user_id = ?`,
		projectID, userID).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
