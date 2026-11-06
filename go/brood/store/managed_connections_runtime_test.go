package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// FEATURE: Сохранение runtime и безопасное представление подключения.
// SPEC: Миграция v5 сохраняет строки и credential, повторное открытие стабильно;
// форматирование и JSON входной структуры не раскрывают секрет или CA.
// IMPLEMENTATION: Additive v6 ALTER и редактированные представления структуры.
// RESULT: IMPLEMENTED — сохранение строк и повторное открытие проверены.
// STATUS: DONE
func TestManagedConnectionRedaction(t *testing.T) {
	c := ManagedConnection{TokenSecret: "private-token-sentinel", CAPEM: "private-ca-sentinel"}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), string(raw)} {
		if strings.Contains(value, "private-") {
			t.Fatal("connection formatting leaks credentials")
		}
	}
}

func TestManagedConnectionV5Migration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "connections.sqlite3")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	// Готовим точную старую v5; колонка есть только после реализации v6.
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('managed_connections') WHERE name='runtime_config'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		if _, err := db.Exec(`ALTER TABLE managed_connections DROP COLUMN runtime_config`); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range []string{"origin", "role", "delegator_id", "parent_uid"} {
		if _, err := db.Exec(`ALTER TABLE managed_actors DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`DROP TABLE pipeline_steps; DROP TABLE pipeline_runs; DROP TABLE project_repositories; DROP TABLE project_secret_files; DROP TABLE project_secret_vars; DROP TABLE project_secret_revisions; DROP TABLE coordination_claims; DROP TABLE command_project_details; INSERT INTO managed_connections VALUES ('c','legacy','https://pve.invalid','a@b!c','keep-secret','keep-ca','n','r','s','v','created','updated'); PRAGMA user_version=5`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := 0; i < 2; i++ {
		s, err = Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 11 {
			t.Fatalf("version=%d err=%v", version, err)
		}
		rows, err := s.ListManagedConnections(ctx)
		if err != nil || len(rows) != 1 || rows[0].Name != "legacy" || rows[0].CAPEM != "keep-ca" || rows[0].CreatedAt != "created" {
			t.Fatal("migration lost row")
		}
		secret, err := s.ManagedConnectionSecret(ctx, "legacy")
		if err != nil || secret != "keep-secret" {
			t.Fatal("migration lost credential")
		}
		s.Close()
	}
}
