// Рои: группы воркеров внутри проектов. Рой — единственная граница
// доступа и scope шины обмена; у роя один владелец.
package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/pem"
	"fmt"

	"golang.org/x/crypto/ssh"
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

// CreateSwarm inserts a swarm and its new Ed25519 SSH keypair atomically.
func (s *Store) CreateSwarm(ctx context.Context, w Swarm) error {
	if w.State == "" {
		w.State = "active"
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate swarm SSH key: %w", err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return fmt.Errorf("encode swarm SSH public key: %w", err)
	}
	sshPrivate, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return fmt.Errorf("encode swarm SSH private key: %w", err)
	}
	publicKey := string(ssh.MarshalAuthorizedKey(sshPublic))
	privateKey := string(pem.EncodeToMemory(sshPrivate))
	return s.WithTx(ctx, func(ctx context.Context) error {
		db := s.conn(ctx)
		// WithTx присоединяется к внешней транзакции. Точка сохранения
		// откатывает обе записи, даже если вызывающий обработает ошибку.
		if _, err := db.ExecContext(ctx, `SAVEPOINT swarm_create`); err != nil {
			return err
		}
		released := false
		defer func() {
			if !released {
				cleanup := context.WithoutCancel(ctx)
				_, _ = db.ExecContext(cleanup, `ROLLBACK TO swarm_create`)
				_, _ = db.ExecContext(cleanup, `RELEASE swarm_create`)
			}
		}()
		_, err := db.ExecContext(ctx, `
			INSERT INTO swarms (id, project_id, owner_id, name, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			w.ID, w.ProjectID, w.OwnerID, w.Name, w.State, nowRFC3339(), nowRFC3339())
		if err != nil {
			return wrapUnique(err, fmt.Sprintf("swarm %q", w.Name))
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO swarm_ssh_keys (swarm_id, public_key, private_key) VALUES (?, ?, ?)`, w.ID, publicKey, privateKey); err != nil {
			return opaqueStorageError(err, "could not store swarm SSH keypair; no swarm was created")
		}
		_, err = db.ExecContext(ctx, `RELEASE swarm_create`)
		released = err == nil
		return err
	})
}

// GetSwarmSSHPublicKey returns the swarm's OpenSSH authorized key line.
func (s *Store) GetSwarmSSHPublicKey(ctx context.Context, swarmID string) (string, error) {
	return s.getSwarmSSHKey(ctx, swarmID, `SELECT public_key FROM swarm_ssh_keys WHERE swarm_id = ?`)
}

// GetSwarmSSHPrivateKey returns the swarm's OpenSSH private key in PEM format.
func (s *Store) GetSwarmSSHPrivateKey(ctx context.Context, swarmID string) (string, error) {
	return s.getSwarmSSHKey(ctx, swarmID, `SELECT private_key FROM swarm_ssh_keys WHERE swarm_id = ?`)
}

func (s *Store) getSwarmSSHKey(ctx context.Context, swarmID, query string) (string, error) {
	var key string
	err := s.conn(ctx).QueryRowContext(ctx, query, swarmID).Scan(&key)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("%w: swarm SSH key", ErrNotFound)
	}
	if err != nil {
		return "", opaqueStorageError(err, "could not read swarm SSH key")
	}
	return key, nil
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
