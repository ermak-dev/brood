package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Фикстура установки содержит активных администраторов, их привязки
// Telegram, подключения Proxmox и пулы, не удалённые на момент экспорта.
// При явном выборе роя экспорт также сохраняет его проект, образы и SSH-ключ.

// FixtureVersion — версия формата фикстуры.
const FixtureVersion = 1

// Fixture — строки таблиц. Значение колонки — строка, целое, число, null
// или {"blob": "<base64>"}.
type Fixture struct {
	Version int            `json:"version"`
	Tables  []FixtureTable `json:"tables"`
}

// FixtureTable — строки одной таблицы фикстуры.
type FixtureTable struct {
	Name string           `json:"name"`
	Rows []map[string]any `json:"rows"`
}

// fixtureTables — таблицы в порядке вставки: внешние ключи ссылаются только
// на предыдущие.
type fixtureTableSpec struct{ name, query string }

var fixtureTables = []fixtureTableSpec{
	{"users", `SELECT * FROM users WHERE is_admin = 1 AND disabled = 0 ORDER BY id`},
	{"sessions", `SELECT * FROM sessions WHERE token_hash IN (
		SELECT session_hash FROM user_telegram_links
		WHERE user_id IN (SELECT id FROM users WHERE is_admin = 1 AND disabled = 0)) ORDER BY token_hash`},
	{"user_telegram_links", `SELECT * FROM user_telegram_links
		WHERE user_id IN (SELECT id FROM users WHERE is_admin = 1 AND disabled = 0) ORDER BY user_id`},
	{"managed_connections", `SELECT * FROM managed_connections ORDER BY name`},
	{"managed_pools", `SELECT * FROM managed_pools WHERE state <> 'deleted' ORDER BY uid`},
}

var fixtureSwarmTables = []fixtureTableSpec{
	{"projects", `SELECT * FROM projects WHERE id=?`},
	{"project_repositories", `SELECT * FROM project_repositories WHERE project_id=?`},
	{"project_images", `SELECT id, project_id, pool_uid, ctid, owner_nonce, state, snapshot, playbook, created_at, updated_at FROM project_images WHERE project_id=? AND state='ready' AND pool_uid IN (SELECT uid FROM managed_pools WHERE state<>'deleted') ORDER BY id`},
	{"swarms", `SELECT * FROM swarms WHERE id=?`},
	{"swarm_ssh_keys", `SELECT * FROM swarm_ssh_keys WHERE swarm_id=?`},
}

// fixtureCleared — колонки, которые ссылаются на состояние вне фикстуры.
var fixtureCleared = map[string][]string{"user_telegram_links": {"default_swarm_id"}}

var fixtureColumn = regexp.MustCompile(`^[a-z_]+$`)

// ExportFixture возвращает фикстуру установки, включая секреты подключений.
func (s *Store) ExportFixture(ctx context.Context) (Fixture, error) {
	return s.exportFixture(ctx, "", "")
}

// ExportFixtureForSwarm exports the installation and one named admin-owned swarm, without agents.
func (s *Store) ExportFixtureForSwarm(ctx context.Context, project, swarm string) (Fixture, error) {
	return s.exportFixture(ctx, project, swarm)
}

func (s *Store) exportFixture(ctx context.Context, project, swarm string) (Fixture, error) {
	f := Fixture{Version: FixtureVersion}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		for _, t := range fixtureTables {
			table, err := s.exportFixtureTable(ctx, t.name, t.query)
			if err != nil {
				return err
			}
			f.Tables = append(f.Tables, table)
		}
		if len(f.Tables[0].Rows) == 0 {
			return fmt.Errorf("fixture export: no enabled administrator")
		}
		if project == "" && swarm == "" {
			return nil
		}
		var projectID, swarmID string
		err := s.conn(ctx).QueryRowContext(ctx, `SELECT p.id,s.id FROM projects p JOIN swarms s ON s.project_id=p.id JOIN users u ON u.id=s.owner_id WHERE p.name=? AND s.name=? AND s.state='active' AND u.is_admin=1 AND u.disabled=0`, project, swarm).Scan(&projectID, &swarmID)
		if err != nil {
			return fmt.Errorf("fixture export: select an active swarm owned by an enabled administrator: %w", err)
		}
		for _, t := range fixtureSwarmTables {
			id := projectID
			if t.name == "swarms" || t.name == "swarm_ssh_keys" {
				id = swarmID
			}
			table, err := s.exportFixtureTable(ctx, t.name, t.query, id)
			if err != nil {
				return err
			}
			f.Tables = append(f.Tables, table)
		}
		return validateFixtureSwarmKeys(f.Tables)
	})
	if err != nil {
		return Fixture{}, err
	}
	return f, nil
}

func (s *Store) exportFixtureTable(ctx context.Context, name, query string, args ...any) (FixtureTable, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return FixtureTable{}, fmt.Errorf("fixture export %s: %w", name, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return FixtureTable{}, err
	}
	table := FixtureTable{Name: name, Rows: []map[string]any{}}
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return FixtureTable{}, fmt.Errorf("fixture export %s: %w", name, err)
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = encodeFixtureValue(values[i])
		}
		for _, col := range fixtureCleared[name] {
			if _, ok := row[col]; ok {
				row[col] = nil
			}
		}
		table.Rows = append(table.Rows, row)
	}
	return table, rows.Err()
}

func encodeFixtureValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return map[string]string{"blob": base64.StdEncoding.EncodeToString(x)}
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	default:
		return x
	}
}

// DecodeFixture разбирает JSON фикстуры: числа остаются точными целыми.
func DecodeFixture(data []byte) (Fixture, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var f Fixture
	if err := dec.Decode(&f); err != nil {
		return Fixture{}, fmt.Errorf("fixture: invalid JSON: %w", err)
	}
	if f.Version != FixtureVersion {
		return Fixture{}, fmt.Errorf("fixture: version %d, want %d", f.Version, FixtureVersion)
	}
	return f, nil
}

// ApplyFixture вставляет строки фикстуры одной транзакцией; существующие
// строки с тем же ключом обновляются.
func (s *Store) ApplyFixture(ctx context.Context, f Fixture) error {
	if err := validateFixtureSwarmKeys(f.Tables); err != nil {
		return err
	}
	tables := append(append([]fixtureTableSpec(nil), fixtureTables...), fixtureSwarmTables...)
	byName := map[string]FixtureTable{}
	for _, t := range f.Tables {
		known := false
		for _, allowed := range tables {
			known = known || allowed.name == t.Name
		}
		if !known {
			return fmt.Errorf("fixture: unknown table %q", t.Name)
		}
		if _, dup := byName[t.Name]; dup {
			return fmt.Errorf("fixture: table %q is listed twice", t.Name)
		}
		byName[t.Name] = t
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, allowed := range tables {
		for i, row := range byName[allowed.name].Rows {
			query, args, err := fixtureInsert(allowed.name, row)
			if err != nil {
				return fmt.Errorf("fixture %s row %d: %w", allowed.name, i, err)
			}
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return fmt.Errorf("fixture %s row %d: %w", allowed.name, i, err)
			}
		}
	}
	return tx.Commit()
}

func validateFixtureSwarmKeys(tables []FixtureTable) error {
	swarms := map[string]bool{}
	pairs := map[string]bool{}
	for _, table := range tables {
		switch table.Name {
		case "swarms":
			for _, row := range table.Rows {
				id, _ := row["id"].(string)
				if id == "" || swarms[id] {
					return fmt.Errorf("fixture: invalid or duplicate swarm identity")
				}
				swarms[id] = true
			}
		case "swarm_ssh_keys":
			for _, row := range table.Rows {
				id, _ := row["swarm_id"].(string)
				pub, _ := row["public_key"].(string)
				priv, _ := row["private_key"].(string)
				signer, err := ssh.ParsePrivateKey([]byte(priv))
				if id == "" || pairs[id] || err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 || strings.TrimSpace(pub) != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) {
					return fmt.Errorf("fixture: invalid or mismatched swarm SSH keypair")
				}
				pairs[id] = true
			}
		}
	}
	if len(swarms) != len(pairs) {
		return fmt.Errorf("fixture: each restored swarm needs exactly one SSH keypair")
	}
	for id := range swarms {
		if !pairs[id] {
			return fmt.Errorf("fixture: restored swarm is missing its SSH keypair")
		}
	}
	return nil
}

func fixtureInsert(table string, row map[string]any) (string, []any, error) {
	if len(row) == 0 {
		return "", nil, fmt.Errorf("empty row")
	}
	cols := make([]string, 0, len(row))
	for col := range row {
		if !fixtureColumn.MatchString(col) {
			return "", nil, fmt.Errorf("invalid column %q", col)
		}
		cols = append(cols, col)
	}
	sort.Strings(cols)
	args := make([]any, len(cols))
	updates := make([]string, len(cols))
	for i, col := range cols {
		v, err := decodeFixtureValue(row[col])
		if err != nil {
			return "", nil, fmt.Errorf("column %s: %w", col, err)
		}
		args[i] = v
		updates[i] = col + " = excluded." + col
	}
	query := "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" +
		strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ") ON CONFLICT DO UPDATE SET " +
		strings.Join(updates, ", ")
	return query, args, nil
}

func decodeFixtureValue(v any) (any, error) {
	switch x := v.(type) {
	case nil, string:
		return x, nil
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, nil
		}
		return x.Float64()
	case float64, int64:
		return x, nil
	case map[string]any:
		raw, ok := x["blob"].(string)
		if !ok || len(x) != 1 {
			return nil, fmt.Errorf("object value must be {\"blob\": base64}")
		}
		return base64.StdEncoding.DecodeString(raw)
	default:
		return nil, fmt.Errorf("unsupported value %T", v)
	}
}
