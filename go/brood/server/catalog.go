// Каталог проектов: явный SQL поверх той же базы store. Пакет store #29
// не даёт перечисления проектов (только CRUD по ID), а прикладному слою
// нужны Projects/ProjectsForUser/Swarms целиком. Чтение только SELECT'ами
// по схеме миграции 0001; запись остаётся методами store.
package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // драйвер уже в зависимостях модуля
)

// Catalog — читающий каталог проектов и роёв той же базы.
type Catalog struct {
	db *sql.DB
}

// catalogDSN — те же прагмы, что у store.Open: WAL и ожидание блокировки.
func catalogDSN(path string) string {
	return "file:" + url.PathEscape(path) +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=synchronous(NORMAL)"
}

// OpenCatalog открывает читающий каталог базы по пути.
func OpenCatalog(path string) (*Catalog, error) {
	db, err := sql.Open("sqlite", catalogDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open catalog %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("open catalog %s: %w", path, err)
	}
	return &Catalog{db: db}, nil
}

// Close закрывает соединение каталога.
func (c *Catalog) Close() error { return c.db.Close() }

// rowProject — строка projects.
type rowProject struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// Projects перечисляет все проекты по имени.
func (c *Catalog) Projects(ctx context.Context) ([]rowProject, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, name, created_at FROM projects ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []rowProject
	for rows.Next() {
		var p rowProject
		var createdAt string
		if err := rows.Scan(&p.ID, &p.Name, &createdAt); err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		p.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectByName ищет проект по имени; ok=false, если нет.
func (c *Catalog) ProjectByName(ctx context.Context, name string) (rowProject, bool, error) {
	var p rowProject
	var createdAt string
	err := c.db.QueryRowContext(ctx,
		`SELECT id, name, created_at FROM projects WHERE name = ?`, name).
		Scan(&p.ID, &p.Name, &createdAt)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("project by name: %w", err)
	}
	p.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return p, false, fmt.Errorf("project by name: %w", err)
	}
	return p, true, nil
}

// ProjectsForUser перечисляет проекты членства пользователя по имени.
func (c *Catalog) ProjectsForUser(ctx context.Context, userID string) ([]rowProject, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT p.id, p.name, p.created_at FROM projects p
		JOIN project_members m ON m.project_id = p.id
		WHERE m.user_id = ? ORDER BY p.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("projects of user: %w", err)
	}
	defer rows.Close()
	var out []rowProject
	for rows.Next() {
		var p rowProject
		var createdAt string
		if err := rows.Scan(&p.ID, &p.Name, &createdAt); err != nil {
			return nil, fmt.Errorf("projects of user: %w", err)
		}
		p.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("projects of user: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// rowSwarm — строка swarms.
type rowSwarm struct {
	ID        string
	ProjectID string
	OwnerID   string
	Name      string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// swarmCols — порядок колонок в списках ниже.
const swarmCols = `id, project_id, owner_id, name, state, created_at, updated_at`

// scanSwarms читает строки swarms.
func scanSwarms(rows *sql.Rows) ([]rowSwarm, error) {
	defer rows.Close()
	var out []rowSwarm
	for rows.Next() {
		var s rowSwarm
		var createdAt, updatedAt string
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.OwnerID, &s.Name, &s.State, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		var err error
		if s.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return nil, err
		}
		if s.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Swarms перечисляет все рои в порядке создания.
func (c *Catalog) Swarms(ctx context.Context) ([]rowSwarm, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+swarmCols+` FROM swarms ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list swarms: %w", err)
	}
	swarms, err := scanSwarms(rows)
	if err != nil {
		return nil, fmt.Errorf("list swarms: %w", err)
	}
	return swarms, nil
}
