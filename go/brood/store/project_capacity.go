package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed project_capacity.sql
var projectCapacitySchema string

// ProjectCapacity stores optional project limits: swarms, workers, CPU and
// memory. Nil limits are unlimited; explicit zero prevents reservations.
type ProjectCapacity struct {
	MaxSwarms        *int64
	MaxActors        *int64
	MaxCPUMillicores *int64
	MaxMemoryBytes   *int64
}

// ValidateProjectCapacity checks limits before persistence.
func ValidateProjectCapacity(capacity ProjectCapacity) error {
	for _, limit := range []struct {
		name  string
		value *int64
	}{
		{"swarms", capacity.MaxSwarms}, {"actors", capacity.MaxActors},
		{"cpu_millicores", capacity.MaxCPUMillicores}, {"memory_bytes", capacity.MaxMemoryBytes},
	} {
		if limit.value != nil && *limit.value < 0 {
			return fmt.Errorf("project %s limit must be nonnegative", limit.name)
		}
	}
	return nil
}

// GetProjectCapacity returns unrestricted defaults for an existing unconfigured project.
func (s *Store) GetProjectCapacity(ctx context.Context, projectID string) (ProjectCapacity, error) {
	var capacity ProjectCapacity
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT c.max_swarms,c.max_actors,c.max_cpu_millicores,c.max_memory_bytes
 FROM projects p LEFT JOIN project_capacity c ON c.project_id=p.id WHERE p.id=?`, projectID).
		Scan(&capacity.MaxSwarms, &capacity.MaxActors, &capacity.MaxCPUMillicores, &capacity.MaxMemoryBytes)
	if err == sql.ErrNoRows {
		return capacity, fmt.Errorf("%w: project %q", ErrNotFound, projectID)
	}
	if err != nil {
		return capacity, err
	}
	if err := ValidateProjectCapacity(capacity); err != nil {
		return ProjectCapacity{}, err
	}
	return capacity, nil
}

// SetProjectCapacity replaces all project limits atomically without tracking usage.
func (s *Store) SetProjectCapacity(ctx context.Context, projectID string, capacity ProjectCapacity) error {
	if err := ValidateProjectCapacity(capacity); err != nil {
		return err
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetProject(ctx, projectID); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO project_capacity
   (project_id,max_swarms,max_actors,max_cpu_millicores,max_memory_bytes) VALUES (?,?,?,?,?)
   ON CONFLICT(project_id) DO UPDATE SET
    max_swarms=excluded.max_swarms,max_actors=excluded.max_actors,
    max_cpu_millicores=excluded.max_cpu_millicores,max_memory_bytes=excluded.max_memory_bytes`,
			projectID, capacity.MaxSwarms, capacity.MaxActors, capacity.MaxCPUMillicores, capacity.MaxMemoryBytes)
		return err
	})
}
