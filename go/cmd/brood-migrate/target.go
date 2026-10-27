// Открытие копии назначения и транзакция импорта без изменения схемы сервера.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"brood.dev/go/brood/store"
	_ "modernc.org/sqlite"
)

func openSQLite(path, mode string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: abs}
	q := url.Values{"mode": {mode}, "_pragma": {"foreign_keys(1)", "busy_timeout(10000)"}, "_txlock": {"immediate"}}
	uri.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func distinctFiles(source, target string) error {
	src, err := os.Stat(source)
	if err != nil {
		return err
	}
	dst, err := os.Stat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if os.SameFile(src, dst) {
		return fmt.Errorf("source and destination must be different files")
	}
	return nil
}

func withTarget(path string, allowExisting bool, fn func(*sql.Tx) error) (err error) {
	created := false
	info, statErr := os.Lstat(path)
	if statErr == nil {
		if !allowExisting {
			return fmt.Errorf("destination exists; use --existing-target yes only for a stopped compatible database copy")
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("destination must be a regular file, not a symlink")
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	} else {
		file, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		created = true
		if e := file.Close(); e != nil {
			_ = os.Remove(path)
			return e
		}
	}
	// Удаляем только созданную нами базу; существующая откатывается SQLite.
	defer func() {
		if err != nil && created {
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				_ = os.Remove(path + suffix)
			}
		}
	}()
	if created {
		st, e := store.Open(context.Background(), path)
		if e != nil {
			return e
		}
		if e := st.Close(); e != nil {
			return e
		}
	}
	db, err := openSQLite(path, "rw")
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := compatibleTarget(tx); err != nil {
		return fmt.Errorf("incompatible destination: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import: %w", err)
	}
	return nil
}

func compatibleTarget(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return err
	}
	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		versions = append(versions, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(versions) < 1 || len(versions) > 2 || versions[0] != "migrations/0001_init.sql" {
		return fmt.Errorf("expected a supported Go schema")
	}
	if len(versions) == 2 && versions[1] != "migrations/0002_swarm_retry.sql" {
		return fmt.Errorf("unsupported Go schema migration")
	}
	for _, query := range []string{
		`SELECT id,username,display_name,password_hash,is_admin,disabled,created_at,updated_at FROM users LIMIT 0`,
		`SELECT id,name,created_at FROM projects LIMIT 0`,
		`SELECT project_id,user_id FROM project_members LIMIT 0`,
		`SELECT id,project_id,owner_id,name,atespace_name,atespace_uid,state,created_at,updated_at FROM swarms LIMIT 0`,
		`SELECT token_hash,user_id,created_at,expires_at,data FROM sessions LIMIT 0`,
	} {
		r, err := tx.Query(query)
		if err != nil {
			return err
		}
		r.Close()
	}
	rows, err = tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	bad := rows.Next()
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if bad {
		return fmt.Errorf("foreign key violations")
	}
	var check string
	if err := tx.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("SQLite integrity check failed")
	}
	return nil
}
