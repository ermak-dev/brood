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
)

// Фикстура установки — профиль администратора, сессия его чата Telegram и
// привязка бота. Временный режим выкладки (BROOD_DEPLOY_MODE=fixture)
// пересоздаёт базу на каждом деплое и заполняет новую базу только ею;
// остальное состояние начинается с нуля.

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
var fixtureTables = []struct{ name, query string }{
	{"users", `SELECT * FROM users WHERE is_admin = 1 AND disabled = 0 ORDER BY id`},
	{"sessions", `SELECT * FROM sessions WHERE token_hash IN (
		SELECT session_hash FROM user_telegram_links
		WHERE user_id IN (SELECT id FROM users WHERE is_admin = 1 AND disabled = 0)) ORDER BY token_hash`},
	{"user_telegram_links", `SELECT * FROM user_telegram_links
		WHERE user_id IN (SELECT id FROM users WHERE is_admin = 1 AND disabled = 0) ORDER BY user_id`},
}

// fixtureCleared — колонки, которые ссылаются на состояние вне фикстуры.
var fixtureCleared = map[string][]string{"user_telegram_links": {"default_swarm_id"}}

var fixtureColumn = regexp.MustCompile(`^[a-z_]+$`)

// ExportFixture читает фикстуру из базы: включённых администраторов, их
// привязки Telegram и сессии этих привязок.
func (s *Store) ExportFixture(ctx context.Context) (Fixture, error) {
	f := Fixture{Version: FixtureVersion}
	for _, t := range fixtureTables {
		table, err := s.exportFixtureTable(ctx, t.name, t.query)
		if err != nil {
			return Fixture{}, err
		}
		f.Tables = append(f.Tables, table)
	}
	if len(f.Tables[0].Rows) == 0 {
		return Fixture{}, fmt.Errorf("fixture export: no enabled administrator")
	}
	return f, nil
}

func (s *Store) exportFixtureTable(ctx context.Context, name, query string) (FixtureTable, error) {
	rows, err := s.db.QueryContext(ctx, query)
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
	byName := map[string]FixtureTable{}
	for _, t := range f.Tables {
		known := false
		for _, allowed := range fixtureTables {
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
	for _, allowed := range fixtureTables {
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
