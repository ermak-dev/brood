// Рои: группы воркеров внутри проектов. Рой — единственная граница
// доступа и scope шины обмена; у роя один владелец.
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Swarm — строка таблицы swarms. State: active или deleted (tombstone).
type Swarm struct {
	ID        string
	ProjectID string
	OwnerID   string
	Name      string
	State     string
	CreatedAt *string
	UpdatedAt *string
}

// swarmCols — список колонок swarms для SELECT.
const swarmCols = `id, project_id, owner_id, name, state, created_at, updated_at`

// scanSwarm читает строку swarms в фиксированном порядке колонок.
func scanSwarm(row interface{ Scan(...any) error }) (Swarm, error) {
	var w Swarm
	var createdAt, updatedAt string
	err := row.Scan(&w.ID, &w.ProjectID, &w.OwnerID, &w.Name, &w.State, &createdAt, &updatedAt)
	if err != nil {
		return w, err
	}
	w.CreatedAt = &createdAt
	w.UpdatedAt = &updatedAt
	return w, nil
}

// CreateSwarm вставляет активный рой. Отсутствующие проект или владелец —
// нарушение внешнего ключа, занятое имя живого роя в проекте — нарушение
// уникальности.
func (s *Store) CreateSwarm(ctx context.Context, w Swarm) error {
	if w.State == "" {
		w.State = "active"
	}
	_, err := s.conn(ctx).ExecContext(ctx, `
		INSERT INTO swarms (id, project_id, owner_id, name, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.ProjectID, w.OwnerID, w.Name, w.State, nowRFC3339(), nowRFC3339())
	if err != nil {
		return wrapUnique(err, fmt.Sprintf("swarm %q", w.Name))
	}
	return nil
}

// GetSwarm возвращает рой по ID.
func (s *Store) GetSwarm(ctx context.Context, id string) (Swarm, error) {
	w, err := scanSwarm(s.conn(ctx).QueryRowContext(ctx,
		`SELECT `+swarmCols+` FROM swarms WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return w, fmt.Errorf("%w: swarm %q", ErrNotFound, id)
	}
	return w, err
}

// listSwarms выполняет выборку по фильтру в порядке создания.
func (s *Store) listSwarms(ctx context.Context, where, arg string) ([]Swarm, error) {
	rows, err := s.conn(ctx).QueryContext(ctx,
		`SELECT `+swarmCols+` FROM swarms WHERE `+where+` ORDER BY created_at, id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var swarms []Swarm
	for rows.Next() {
		w, err := scanSwarm(rows)
		if err != nil {
			return nil, err
		}
		swarms = append(swarms, w)
	}
	return swarms, rows.Err()
}

// ListSwarmsByProject возвращает рои проекта в порядке создания.
func (s *Store) ListSwarmsByProject(ctx context.Context, projectID string) ([]Swarm, error) {
	swarms, err := s.listSwarms(ctx, `project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list swarms of project %s: %w", projectID, err)
	}
	return swarms, nil
}

// ListSwarmsByOwner возвращает рои владельца в порядке создания.
func (s *Store) ListSwarmsByOwner(ctx context.Context, ownerID string) ([]Swarm, error) {
	swarms, err := s.listSwarms(ctx, `owner_id = ?`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list swarms of owner %s: %w", ownerID, err)
	}
	return swarms, nil
}

// SetSwarmState меняет состояние роя (active/deleted).
func (s *Store) SetSwarmState(ctx context.Context, id, state string) error {
	return s.updateSwarm(ctx, id, state, `state = ?, updated_at = ?`)
}

// SetSwarmOwner передаёт рой другому пользователю.
func (s *Store) SetSwarmOwner(ctx context.Context, id, ownerID string) error {
	return s.updateSwarm(ctx, id, ownerID, `owner_id = ?, updated_at = ?`)
}

// updateSwarm выполняет UPDATE swarms с данным фрагментом SET.
func (s *Store) updateSwarm(ctx context.Context, id string, arg any, set string) error {
	res, err := s.conn(ctx).ExecContext(ctx,
		`UPDATE swarms SET `+set+` WHERE id = ?`, arg, nowRFC3339(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: swarm %q", ErrNotFound, id)
	}
	return nil
}
