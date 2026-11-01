package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Подключения Proxmox, создаваемые из панели (#118). Секрет токена
// write-only: пишется один раз и не читается обратно ни списком, ни DTO.

const managedConnectionCols = `uid,name,endpoint,token_id,token_secret,ca_pem,node,resource_pool,storage,network,created_at,updated_at`

// ManagedConnection — строка каталога. TokenSecret заполняется только при
// создании: чтение из базы возвращает его пустым.
type ManagedConnection struct {
	UID          string
	Name         string
	Endpoint     string
	TokenID      string
	TokenSecret  string
	CAPEM        string
	Node         string
	ResourcePool string
	Storage      string
	Network      string
	CreatedAt    string
	UpdatedAt    string
}

// CreateManagedConnection сохраняет подключение; имя уникально.
func (s *Store) CreateManagedConnection(ctx context.Context, c ManagedConnection) (ManagedConnection, error) {
	if c.UID == "" {
		c.UID = NewID()
	}
	now := time.Now().UTC().Format(time.RFC3339)
	c.CreatedAt, c.UpdatedAt = now, now
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO managed_connections (`+managedConnectionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.UID, c.Name, c.Endpoint, c.TokenID, c.TokenSecret, c.CAPEM, c.Node, c.ResourcePool, c.Storage, c.Network, c.CreatedAt, c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	if err != nil {
		return ManagedConnection{}, fmt.Errorf("managed connection exists or invalid: %w", err)
	}
	c.TokenSecret = ""
	return c, nil
}

// ListManagedConnections перечисляет подключения без секретов.
func (s *Store) ListManagedConnections(ctx context.Context) ([]ManagedConnection, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT uid,name,endpoint,token_id,'',ca_pem,node,resource_pool,storage,network,created_at,updated_at FROM managed_connections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManagedConnection
	for rows.Next() {
		var c ManagedConnection
		if err := rows.Scan(&c.UID, &c.Name, &c.Endpoint, &c.TokenID, &c.TokenSecret, &c.CAPEM, &c.Node, &c.ResourcePool, &c.Storage, &c.Network, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ManagedConnectionSecret возвращает секрет по имени подключения; чтение
// только серверным кодом создания клиента PVE, не API.
func (s *Store) ManagedConnectionSecret(ctx context.Context, name string) (string, error) {
	var secret string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT token_secret FROM managed_connections WHERE name=?`, name).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: managed connection", ErrNotFound)
	}
	return secret, err
}

// DeleteManagedConnection удаляет подключение по UID.
func (s *Store) DeleteManagedConnection(ctx context.Context, uid string) error {
	res, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM managed_connections WHERE uid=?`, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: managed connection", ErrNotFound)
	}
	return nil
}
