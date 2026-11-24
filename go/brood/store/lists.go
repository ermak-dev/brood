// Перечисления проектов и роёв целиком: прикладному слою и плоским спискам
// панели. Чтение идёт тем же соединением и той же транзакцией из ctx, что
// и остальные запросы store.
package store

import (
	"context"
	"fmt"
)

// ListProjects returns every project ordered by name.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	return s.listProjects(ctx, `SELECT id, name, created_at FROM projects ORDER BY name`)
}

// ListMemberProjects returns the projects the user is a member of, ordered by name.
func (s *Store) ListMemberProjects(ctx context.Context, userID string) ([]Project, error) {
	return s.listProjects(ctx, `
		SELECT p.id, p.name, p.created_at FROM projects p
		JOIN project_members m ON m.project_id = p.id
		WHERE m.user_id = ? ORDER BY p.name`, userID)
}

// listProjects читает строки projects в порядке колонок id, name, created_at.
func (s *Store) listProjects(ctx context.Context, query string, args ...any) ([]Project, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var createdAt string
		if err := rows.Scan(&p.ID, &p.Name, &createdAt); err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		p.CreatedAt = &createdAt
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListSwarms returns every swarm, deleted ones included, in creation order.
func (s *Store) ListSwarms(ctx context.Context) ([]Swarm, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT `+swarmCols+` FROM swarms ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list swarms: %w", err)
	}
	defer rows.Close()
	var out []Swarm
	for rows.Next() {
		sw, err := scanSwarm(rows)
		if err != nil {
			return nil, fmt.Errorf("list swarms: %w", err)
		}
		out = append(out, sw)
	}
	return out, rows.Err()
}
