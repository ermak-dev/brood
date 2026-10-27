// Офлайн-перенос auth_user с управляемым сбросом паролей.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"brood.dev/go/brood/auth"
)

type djUser struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	IsSuperuser bool   `json:"is_superuser"`
	IsActive    bool   `json:"is_active"`
}

type usersOptions struct {
	DjangoDB, UsersJSON, DB, SourceID, BootstrapAdmin, Report string
	ExistingTarget                                            bool
}

type resetEntry struct {
	Username     string `json:"username"`
	Action       string `json:"action"`
	SourceActive bool   `json:"source_active"`
}

type resetReport struct {
	BootstrapAdmin string       `json:"bootstrap_admin"`
	Instructions   string       `json:"instructions"`
	Users          []resetEntry `json:"users"`
}

func cmdUsers(argv []string) error {
	f, err := parseFlags(argv, "django-db", "users-json", "db", "source-id", "bootstrap-admin", "out-report", "existing-target")
	if err != nil {
		return err
	}
	for _, name := range []string{"db", "source-id", "bootstrap-admin", "out-report"} {
		if _, err := f.get(name); err != nil {
			return err
		}
	}
	allow, err := existingTargetFlag(f)
	if err != nil {
		return err
	}
	return importUsers(usersOptions{DjangoDB: f.values["django-db"], UsersJSON: f.values["users-json"], DB: f.values["db"], SourceID: f.values["source-id"], BootstrapAdmin: f.values["bootstrap-admin"], Report: f.values["out-report"], ExistingTarget: allow})
}

// importedUserID связывает неизменяемые координаты источника, а не имя входа.
func importedUserID(source string, id int64) string {
	return fmt.Sprintf("django:%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", source, id))))
}

func importUsers(o usersOptions) error {
	if strings.TrimSpace(o.SourceID) == "" || strings.TrimSpace(o.BootstrapAdmin) == "" || o.DB == "" || o.Report == "" {
		return fmt.Errorf("db, source-id, bootstrap-admin and out-report are required")
	}
	if (o.DjangoDB == "") == (o.UsersJSON == "") {
		return fmt.Errorf("choose exactly one of --django-db and --users-json")
	}
	source := o.DjangoDB
	if source == "" {
		source = o.UsersJSON
	}
	if err := distinctFiles(source, o.DB); err != nil {
		return err
	}
	var users []djUser
	var err error
	if o.DjangoDB != "" {
		users, err = readDjangoUsers(o.DjangoDB)
	} else {
		err = readJSON(o.UsersJSON, &users)
	}
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return fmt.Errorf("source has no users")
	}
	ids, names := map[int64]bool{}, map[string]bool{}
	for _, u := range users {
		if u.ID <= 0 || strings.TrimSpace(u.Username) == "" || ids[u.ID] || names[u.Username] {
			return fmt.Errorf("source needs unique positive IDs and nonempty unique usernames")
		}
		if u.Username == o.BootstrapAdmin {
			return fmt.Errorf("bootstrap-admin must be separate from all imported usernames; choose a new recovery name")
		}
		ids[u.ID] = true
		names[u.Username] = true
	}
	report := resetReport{BootstrapAdmin: o.BootstrapAdmin, Instructions: "Stop the server and work only on database copies. Create this separate admin with brood bootstrap-admin against the target copy if absent. Sign in as that admin, POST /api/admin/users/{username}/password-reset with new_password, then explicitly approve each account with POST /api/admin/users/{username}/enable. Source-inactive accounts must remain disabled. No Django sessions or password hashes are copied; all target sessions are invalidated."}
	reportWritten := false
	err = withTarget(o.DB, o.ExistingTarget, func(tx *sql.Tx) error {
		var admin, disabled bool
		err := tx.QueryRow(`SELECT is_admin,disabled FROM users WHERE username=?`, o.BootstrapAdmin).Scan(&admin, &disabled)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (!admin || disabled) {
			return fmt.Errorf("bootstrap-admin already exists but is not an active admin; choose an unused recovery name")
		}
		for _, u := range users {
			id := importedUserID(o.SourceID, u.ID)
			var existingID, existingName string
			err := tx.QueryRow(`SELECT id,username FROM users WHERE id=? OR username=?`, id, u.Username).Scan(&existingID, &existingName)
			if err == nil {
				if existingID != id || existingName != u.Username {
					return fmt.Errorf("source identity conflict for user %q; refusing to merge or rename", u.Username)
				}
				// Оба ключа должны указывать ровно на одну строку.
				var n int
				if err := tx.QueryRow(`SELECT count(*) FROM users WHERE id=? OR username=?`, id, u.Username).Scan(&n); err != nil {
					return err
				}
				if n != 1 {
					return fmt.Errorf("ambiguous source identity for %q", u.Username)
				}
				report.Users = append(report.Users, resetEntry{u.Username, "EXISTING USER PRESERVED", u.IsActive})
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			secret, err := newRandomPassword()
			if err != nil {
				return err
			}
			hash, err := auth.HashPassword(secret, auth.DefaultArgon2())
			if err != nil {
				return err
			}
			now := time.Now().UTC().Format(time.RFC3339)
			if _, err := tx.Exec(`INSERT INTO users(id,username,display_name,password_hash,is_admin,disabled,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)`, id, u.Username, u.Username, hash, u.IsSuperuser, now, now); err != nil {
				return fmt.Errorf("import user %q: %w", u.Username, err)
			}
			report.Users = append(report.Users, resetEntry{u.Username, "REQUIRED ADMIN PASSWORD RESET", u.IsActive})
		}
		if _, err := tx.Exec(`DELETE FROM sessions`); err != nil {
			return err
		}
		// Файл публикуется до commit, чтобы отказ записи откатил базу.
		if err := writeReport(o.Report, report); err != nil {
			return err
		}
		reportWritten = true
		return nil
	})
	if err != nil && reportWritten {
		_ = os.Remove(o.Report)
	}
	return err
}

func readDjangoUsers(path string) ([]djUser, error) {
	db, err := openSQLite(path, "ro")
	if err != nil {
		return nil, fmt.Errorf("open Django database: %w", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), `SELECT id,username,is_superuser,is_active FROM auth_user ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read auth_user: %w", err)
	}
	defer rows.Close()
	var users []djUser
	for rows.Next() {
		var u djUser
		if err := rows.Scan(&u.ID, &u.Username, &u.IsSuperuser, &u.IsActive); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}
