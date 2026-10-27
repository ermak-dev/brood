// Фикстуры тестов brood-migrate: база Django (SQLite, auth_user) и файл
// отображения atespace.
package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // драйвер уже в зависимостях модуля
)

// mustExec выполняет DDL/insert отказоустойчиво.
func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("fixture exec: %v", err)
	}
}

// djFixture пишет базу Django со строками auth_user: (username, is_superuser,
// is_active); пароль -- корректный pbkdf2_sha256, миграцию не интересует.
func djFixture(t *testing.T, rows [][3]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "django.sqlite3")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE auth_user (
		id INTEGER PRIMARY KEY,
		password TEXT NOT NULL,
		last_login DATETIME,
		is_superuser INTEGER NOT NULL,
		username TEXT NOT NULL UNIQUE,
		first_name TEXT NOT NULL,
		last_name TEXT NOT NULL,
		email TEXT NOT NULL,
		is_staff INTEGER NOT NULL,
		is_active INTEGER NOT NULL,
		date_joined DATETIME NOT NULL)`)
	hash := "pbkdf2_sha256$390000$c2FsdA==$aGFzaA=="
	for _, row := range rows {
		mustExec(t, db, `INSERT INTO auth_user
			(username, password, is_superuser, is_active, email, first_name,
			 last_name, is_staff, date_joined, last_login)
			VALUES (?, ?, ?, ?, '', '', '', 0, '2026-01-01', NULL)`,
			row[0], hash, row[1], row[2])
	}
	return path
}

// mappingFixture пишет файл отображения JSON и возвращает его путь.
func mappingFixture(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mapping.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// errFatal -- сокращение: отказ теста с сообщением ошибки.
func errFatal(t *testing.T, what string, err error) {
	t.Helper()
	t.Fatalf("%s: %v", what, err)
}
