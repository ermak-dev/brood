package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"
)

// Подключения Proxmox, создаваемые из панели (#118). Секрет токена
// write-only: пишется один раз и не читается обратно ни списком, ни DTO.

const managedConnectionCols = `uid,name,endpoint,token_id,token_secret,ca_pem,node,resource_pool,storage,network,created_at,updated_at,runtime_config`

// ManagedConnection is a catalog row with write-only input credentials.
// RuntimeJSON contains only the approved template and aggregate capacity.
type ManagedConnection struct {
	UID          string
	Name         string
	Endpoint     string
	TokenID      string
	TokenSecret  string `json:"-"`
	CAPEM        string `json:"-"`
	RuntimeJSON  string
	Node         string
	ResourcePool string
	Storage      string
	Network      string
	CreatedAt    string
	UpdatedAt    string
}

// String redacts connection input in diagnostics.
func (c ManagedConnection) String() string { return "ManagedConnection{redacted}" }

// GoString redacts connection input in Go-syntax diagnostics.
func (c ManagedConnection) GoString() string { return c.String() }

// Format serves every verb: String and GoString cover only some, and the rest
// would print the fields. %p bypasses Format and prints an address only.
func (c ManagedConnection) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, c.String()) }

// CreateManagedConnection persists a uniquely named connection without echoing its token.
func (s *Store) CreateManagedConnection(ctx context.Context, c ManagedConnection) (ManagedConnection, error) {
	if c.UID == "" {
		c.UID = NewID()
	}
	now := time.Now().UTC().Format(time.RFC3339)
	c.CreatedAt, c.UpdatedAt = now, now
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO managed_connections (`+managedConnectionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.UID, c.Name, c.Endpoint, c.TokenID, c.TokenSecret, c.CAPEM, c.Node, c.ResourcePool, c.Storage, c.Network, c.CreatedAt, c.UpdatedAt, c.RuntimeJSON)
	if err != nil {
		return ManagedConnection{}, errors.New("managed connection exists or is invalid")
	}
	c.TokenSecret = ""
	return c, nil
}

// ListManagedConnections lists connections without token secrets.
func (s *Store) ListManagedConnections(ctx context.Context) ([]ManagedConnection, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, `SELECT uid,name,endpoint,token_id,'',ca_pem,node,resource_pool,storage,network,created_at,updated_at,runtime_config FROM managed_connections ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManagedConnection
	for rows.Next() {
		var c ManagedConnection
		if err := rows.Scan(&c.UID, &c.Name, &c.Endpoint, &c.TokenID, &c.TokenSecret, &c.CAPEM, &c.Node, &c.ResourcePool, &c.Storage, &c.Network, &c.CreatedAt, &c.UpdatedAt, &c.RuntimeJSON); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ManagedConnectionSecret reads the token only for internal PVE client construction.
func (s *Store) ManagedConnectionSecret(ctx context.Context, name string) (string, error) {
	var secret string
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT token_secret FROM managed_connections WHERE name=?`, name).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: managed connection", ErrNotFound)
	}
	return secret, err
}

// DeleteManagedConnection removes an unused connection by UID.
func (s *Store) DeleteManagedConnection(ctx context.Context, uid string) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		var name string
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT name FROM managed_connections WHERE uid=?`, uid).Scan(&name); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return errors.New("managed connection unavailable")
		}
		var used bool
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_pools p WHERE p.connection=? AND (p.state<>'deleted' OR EXISTS(SELECT 1 FROM managed_actors a WHERE a.pool_uid=p.uid AND a.phase<>'deleted')))`, name).Scan(&used); err != nil {
			return errors.New("managed connection usage unavailable")
		}
		if used {
			return ErrManagedChildren
		}
		_, err := s.conn(ctx).ExecContext(ctx, `DELETE FROM managed_connections WHERE uid=?`, uid)
		if err != nil {
			return errors.New("managed connection deletion failed")
		}
		return nil
	})
}
