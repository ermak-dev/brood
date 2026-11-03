package store

import (
	"context"
	"database/sql"
	"fmt"

	"brood.dev/go/brood/gitorigin"
)

// storedOrigin допускает пустой origin или уже канонический HTTPS origin:
// хранилище не нормализует ввод молча, это делает вызывающий.
func storedOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	canonical, err := gitorigin.Canonical(origin)
	return err == nil && canonical == origin
}

// ProjectRepository returns the project's selected installation remote, or ""
// when none is selected. A missing project is ErrNotFound.
// One statement reads both facts from the same snapshot, without a write lock.
func (s *Store) ProjectRepository(ctx context.Context, projectID string) (string, error) {
	var origin sql.NullString
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT r.origin FROM projects p LEFT JOIN project_repositories r ON r.project_id = p.id WHERE p.id = ?`, projectID).Scan(&origin)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("%w: project", ErrNotFound)
	}
	return origin.String, err
}

// SetProjectRepository replaces the project's repository; "" clears it.
// Callers authorize and check the origin against installation remotes; the
// store accepts only canonical origins and existing projects.
func (s *Store) SetProjectRepository(ctx context.Context, projectID, origin string) error {
	if !storedOrigin(origin) {
		return fmt.Errorf("project repository must be empty or a canonical HTTPS origin")
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetProject(ctx, projectID); err != nil {
			return err
		}
		if origin == "" {
			_, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM project_repositories WHERE project_id=?`, projectID)
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO project_repositories(project_id,origin) VALUES (?,?) ON CONFLICT(project_id) DO UPDATE SET origin=excluded.origin`, projectID, origin)
		return err
	})
}
