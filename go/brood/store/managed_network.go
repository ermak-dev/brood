package store

import (
	"context"
	"database/sql"
	"fmt"

	"brood.dev/go/brood/proxmox/egress"
)

// ProjectManagedNetwork returns the selected canonical policy, or empty for legacy.
// It reads configuration, not execution authority. A missing project is ErrNotFound.
func (s *Store) ProjectManagedNetwork(ctx context.Context, projectID string) (string, error) {
	var policy sql.NullString
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT n.policy FROM projects p LEFT JOIN project_managed_networks n ON n.project_id=p.id WHERE p.id=?`, projectID).Scan(&policy)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("%w: project", ErrNotFound)
	}
	return policy.String, err
}

func normalizeManagedNetwork(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	policy, err := egress.Decode([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("invalid managed network policy")
	}
	encoded, err := policy.Encode()
	if err != nil {
		return "", fmt.Errorf("invalid managed network policy")
	}
	return string(encoded), nil
}

// Вызывается только внутри авторизованной транзакции SyncProject.
func (s *Store) setProjectManagedNetwork(ctx context.Context, projectID, policy string) error {
	if policy == "" {
		_, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM project_managed_networks WHERE project_id=?`, projectID)
		return err
	}
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO project_managed_networks(project_id,policy) VALUES (?,?) ON CONFLICT(project_id) DO UPDATE SET policy=excluded.policy`, projectID, policy)
	return err
}

// Snapshot создаётся в той же транзакции, что новый UID. У старого актора без
// строки snapshot всегда legacy; чтение никогда не выводит policy из default.
func (s *Store) snapshotManagedNetwork(ctx context.Context, actorUID, policy string) error {
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO managed_actor_networks(actor_uid,policy) VALUES (?,?)`, actorUID, policy)
	return err
}
