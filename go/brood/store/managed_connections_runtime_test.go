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
// SPEC: Подключения переживают переоткрытие базы; чужая версия схемы
// отвергается без потери credential; форматирование и JSON входной
// структуры не раскрывают секрет или CA.
// Миграций больше нет (#205).
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

// TestManagedConnectionForeignVersionRefused: миграций нет (#205); база
// версии 5 не обновляется, а отвергается без изменения схемы, версии
// и данных подключений.
func TestManagedConnectionForeignVersionRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "connections.sqlite3")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateManagedConnection(ctx, ManagedConnection{Name: "pve-main", Endpoint: "https://pve.invalid:8006", TokenID: "op@pve!brood", TokenSecret: "keep-secret", CAPEM: "keep-ca", Node: "n", ResourcePool: "r", Storage: "s", Network: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version=5`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, s.db)
	s.Close()
	if st, err := Open(ctx, path); err == nil {
		st.Close()
		t.Fatal("previous managed connection version accepted")
	} else if !strings.Contains(err.Error(), "incompatible database schema") {
		t.Fatal(err)
	}
	if err := assertDatabaseUnchanged(t, path, 5, before); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var secret string
	if err := db.QueryRow(`SELECT token_secret FROM managed_connections WHERE name='pve-main'`).Scan(&secret); err != nil || secret != "keep-secret" {
		t.Fatalf("credential changed: %q %v", secret, err)
	}
}
