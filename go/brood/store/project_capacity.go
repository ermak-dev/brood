package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

//go:embed project_capacity.sql
var projectCapacitySchema string

// ProjectCapacity stores required worker labels and optional shard limits.
// Nil limits are unlimited; explicit zero prevents reservations.
type ProjectCapacity struct {
	WorkerSelector   map[string]string
	MaxSwarms        *int64
	MaxActors        *int64
	MaxCPUMillicores *int64
	MaxMemoryBytes   *int64
}

var capacityLabelPart = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$`)
var capacityDNSPart = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

func validateCapacityLabels(labels map[string]string) error {
	if len(labels) > 10 {
		return fmt.Errorf("shard worker selector permits at most 10 labels")
	}
	for key, value := range labels {
		parts := strings.Split(key, "/")
		name := parts[len(parts)-1]
		if len(parts) > 2 || len(name) > 63 || !capacityLabelPart.MatchString(name) {
			return fmt.Errorf("invalid shard worker label key %q", key)
		}
		if len(parts) == 2 {
			if len(parts[0]) > 253 {
				return fmt.Errorf("invalid shard worker label prefix")
			}
			for _, part := range strings.Split(parts[0], ".") {
				if len(part) > 63 || !capacityDNSPart.MatchString(part) {
					return fmt.Errorf("invalid shard worker label prefix")
				}
			}
		}
		if len(value) > 63 || (value != "" && !capacityLabelPart.MatchString(value)) {
			return fmt.Errorf("invalid shard worker label value for %q", key)
		}
	}
	return nil
}

// ValidateProjectCapacity checks limits and native worker labels before persistence.
func ValidateProjectCapacity(capacity ProjectCapacity) error {
	for _, limit := range []struct {
		name  string
		value *int64
	}{
		{"swarms", capacity.MaxSwarms}, {"actors", capacity.MaxActors},
		{"cpu_millicores", capacity.MaxCPUMillicores}, {"memory_bytes", capacity.MaxMemoryBytes},
	} {
		if limit.value != nil && *limit.value < 0 {
			return fmt.Errorf("shard %s limit must be nonnegative", limit.name)
		}
	}
	return validateCapacityLabels(capacity.WorkerSelector)
}

// GetProjectCapacity returns unrestricted defaults for an existing unconfigured project.
func (s *Store) GetProjectCapacity(ctx context.Context, projectID string) (ProjectCapacity, error) {
	var capacity ProjectCapacity
	var encoded sql.NullString
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT c.worker_selector,c.max_swarms,c.max_actors,c.max_cpu_millicores,c.max_memory_bytes
 FROM projects p LEFT JOIN project_capacity c ON c.project_id=p.id WHERE p.id=?`, projectID).
		Scan(&encoded, &capacity.MaxSwarms, &capacity.MaxActors, &capacity.MaxCPUMillicores, &capacity.MaxMemoryBytes)
	if err == sql.ErrNoRows {
		return capacity, fmt.Errorf("%w: project %q", ErrNotFound, projectID)
	}
	if err != nil {
		return capacity, err
	}
	if encoded.Valid {
		var labels map[string]any
		if err := json.Unmarshal([]byte(encoded.String), &labels); err != nil || labels == nil {
			return ProjectCapacity{}, fmt.Errorf("invalid stored shard worker selector")
		}
		if len(labels) > 0 {
			capacity.WorkerSelector = make(map[string]string, len(labels))
		}
		for key, raw := range labels {
			value, ok := raw.(string)
			if !ok {
				return ProjectCapacity{}, fmt.Errorf("stored shard worker selector values must be strings")
			}
			capacity.WorkerSelector[key] = value
		}
	}
	if err := ValidateProjectCapacity(capacity); err != nil {
		return ProjectCapacity{}, err
	}
	return capacity, nil
}

// SetProjectCapacity replaces all shard properties atomically without tracking usage.
func (s *Store) SetProjectCapacity(ctx context.Context, projectID string, capacity ProjectCapacity) error {
	if err := ValidateProjectCapacity(capacity); err != nil {
		return err
	}
	labels := capacity.WorkerSelector
	if labels == nil {
		labels = map[string]string{}
	}
	encoded, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.GetProject(ctx, projectID); err != nil {
			return err
		}
		_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO project_capacity
   (project_id,worker_selector,max_swarms,max_actors,max_cpu_millicores,max_memory_bytes) VALUES (?,?,?,?,?,?)
   ON CONFLICT(project_id) DO UPDATE SET worker_selector=excluded.worker_selector,
    max_swarms=excluded.max_swarms,max_actors=excluded.max_actors,
    max_cpu_millicores=excluded.max_cpu_millicores,max_memory_bytes=excluded.max_memory_bytes`,
			projectID, string(encoded), capacity.MaxSwarms, capacity.MaxActors, capacity.MaxCPUMillicores, capacity.MaxMemoryBytes)
		return err
	})
}

// Регистрация лимитов в общей схеме — schemaModules ("project-capacity")
// и verifyProjectCapacitySchema в registry.go (#205).
