// Рои: личные atespace-области пользователей внутри проектов.
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Swarm — строка таблицы swarms. state — provisioning/active/deleting/
// error, физическая схема состояний остаётся прикладной.
type Swarm struct {
	ID           string
	ProjectID    string
	OwnerID      string
	Name         string
	AtespaceName string
	AtespaceUID  *string
	State        string
	Retry        string
	CreatedAt    *string
	UpdatedAt    *string
}

// swarmCols — список колонок swarms для SELECT.
const swarmCols = `id, project_id, owner_id, name, atespace_name, atespace_uid, state, created_at, updated_at, retry`

// scanSwarm читает строку swarms в фиксированном порядке колонок.
func scanSwarm(row interface{ Scan(...any) error }) (Swarm, error) {
	var w Swarm
	var createdAt, updatedAt string
	err := row.Scan(&w.ID, &w.ProjectID, &w.OwnerID, &w.Name, &w.AtespaceName,
		&w.AtespaceUID, &w.State, &createdAt, &updatedAt, &w.Retry)
	if err != nil {
		return w, err
	}
	w.CreatedAt = &createdAt
	w.UpdatedAt = &updatedAt
	return w, nil
}

// CreateSwarm вставляет рой; store проставляет created_at, updated_at
// и дефолтное состояние provisioning. Отсутствующие проект или
// владелец — нарушение внешнего ключа, занятый atespace_name —
// нарушение уникальности.
func (s *Store) CreateSwarm(ctx context.Context, w Swarm) error {
	if w.State == "" {
		w.State = "provisioning"
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.conn(ctx).ExecContext(ctx, `
			INSERT INTO swarms (id, project_id, owner_id, name, atespace_name, atespace_uid, state, created_at, updated_at, retry)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			w.ID, w.ProjectID, w.OwnerID, w.Name, w.AtespaceName, w.AtespaceUID,
			w.State, nowRFC3339(), nowRFC3339(), w.Retry)
		if err != nil {
			return wrapUnique(err, fmt.Sprintf("swarm %q or atespace %q", w.Name, w.AtespaceName))
		}
		_, err = s.conn(ctx).ExecContext(ctx,
			`INSERT INTO swarm_members (swarm_id, user_id, role) VALUES (?, ?, 'owner')`, w.ID, w.OwnerID)
		return err
	})
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

// GetSwarmByAtespace возвращает рой по имени его atespace.
func (s *Store) GetSwarmByAtespace(ctx context.Context, atespaceName string) (Swarm, error) {
	w, err := scanSwarm(s.conn(ctx).QueryRowContext(ctx,
		`SELECT `+swarmCols+` FROM swarms WHERE atespace_name = ?`, atespaceName))
	if err == sql.ErrNoRows {
		return w, fmt.Errorf("%w: atespace %q", ErrNotFound, atespaceName)
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

// SetSwarmState меняет состояние роя (provisioning/active/deleting/error).
func (s *Store) SetSwarmState(ctx context.Context, id, state string) error {
	return s.updateSwarm(ctx, id, state, `state = ?, updated_at = ?`)
}

// SetSwarmProgress сохраняет состояние вместе с намерением повтора атомарно.
func (s *Store) SetSwarmProgress(ctx context.Context, id, state, retry string) error {
	result, err := s.conn(ctx).ExecContext(ctx,
		`UPDATE swarms SET state = ?, retry = ?, updated_at = ? WHERE id = ?`, state, retry, nowRFC3339(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("%w: swarm %q", ErrNotFound, id)
	}
	return nil
}

// SetSwarmAtespaceUID сохраняет UID созданного atespace; nil стирает.
func (s *Store) SetSwarmAtespaceUID(ctx context.Context, id string, uid *string) error {
	return s.updateSwarm(ctx, id, uid, `atespace_uid = ?, updated_at = ?`)
}

// SetSwarmOwner передаёт рой другому пользователю (административная
// операция с проверкой членства на прикладном слое).
func (s *Store) SetSwarmOwner(ctx context.Context, id, ownerID string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		current, err := s.GetSwarm(ctx, id)
		if err != nil {
			return err
		}
		if err := s.SetSwarmMember(ctx, id, ownerID, "owner"); err != nil {
			return err
		}
		if current.OwnerID != ownerID {
			if err := s.RemoveSwarmMember(ctx, id, current.OwnerID); err != nil {
				return err
			}
		}
		return s.updateSwarm(ctx, id, ownerID, `owner_id = ?, updated_at = ?`)
	})
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
