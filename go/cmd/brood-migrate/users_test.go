// Проверки офлайн-импорта и управляемого сброса паролей.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

func userOptions(t *testing.T, source, target string) usersOptions {
	t.Helper()
	return usersOptions{DjangoDB: source, DB: target, SourceID: "installation-one", BootstrapAdmin: "recovery-admin", Report: filepath.Join(t.TempDir(), "report.json")}
}

func TestUsersManagedResetAndBootstrapRecovery(t *testing.T) {
	source := djFixture(t, [][3]any{{"root", 1, 1}, {"alice", 0, 1}, {"inactive", 0, 0}})
	target := filepath.Join(t.TempDir(), "go.db")
	opts := userOptions(t, source, target)
	if err := importUsers(opts); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(opts.Report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "token") || !strings.Contains(string(raw), "REQUIRED ADMIN PASSWORD RESET") {
		t.Fatalf("misleading report: %s", raw)
	}
	st, err := store.Open(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, name := range []string{"root", "alice", "inactive"} {
		u, err := st.GetUserByUsername(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		if !u.Disabled {
			t.Fatal("import enabled a user")
		}
		ok, err := auth.VerifyPassword("pbkdf2_sha256$390000$c2FsdA==$aGFzaA==", u.PasswordHash)
		if err != nil || ok {
			t.Fatalf("invalid random Argon2 password: %v %v", ok, err)
		}
	}
	h, _ := auth.HashPassword("known admin password", auth.DefaultArgon2())
	created, err := st.BootstrapAdmin(t.Context(), opts.BootstrapAdmin, h)
	if err != nil || !created {
		t.Fatalf("bootstrap recovery blocked: %v %v", created, err)
	}
	u, _ := st.GetUserByUsername(t.Context(), "alice")
	reset, _ := auth.HashPassword("admin approved password", auth.DefaultArgon2())
	if err := st.SetPasswordHash(t.Context(), u.ID, reset); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserDisabled(t.Context(), u.ID, false); err != nil {
		t.Fatal(err)
	}
	u, _ = st.GetUser(t.Context(), u.ID)
	ok, err := auth.VerifyPassword("admin approved password", u.PasswordHash)
	if err != nil || !ok || u.Disabled {
		t.Fatal("managed reset not usable")
	}
	admin, _ := st.GetUserByUsername(t.Context(), opts.BootstrapAdmin)
	if err := st.SetUserDisabled(t.Context(), admin.ID, true); err == nil {
		t.Fatal("last admin guard lost")
	}
	info, _ := os.Stat(opts.Report)
	if info.Mode().Perm() != 0600 {
		t.Fatal("report permissions")
	}
}

func TestUsersRerunPreservesCredentialsAndInvalidatesSessions(t *testing.T) {
	source := djFixture(t, [][3]any{{"alice", 0, 1}})
	target := filepath.Join(t.TempDir(), "go.db")
	opts := userOptions(t, source, target)
	if err := importUsers(opts); err != nil {
		t.Fatal(err)
	}
	st, _ := store.Open(t.Context(), target)
	u, _ := st.GetUserByUsername(t.Context(), "alice")
	h, _ := auth.HashPassword("changed", auth.DefaultArgon2())
	if err := st.SetPasswordHash(t.Context(), u.ID, h); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserDisabled(t.Context(), u.ID, false); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db := openSQL(t, target)
	mustExec(t, db, `INSERT INTO sessions(token_hash,user_id,created_at) VALUES('old-session',?,'now')`, u.ID)
	db.Close()
	opts.ExistingTarget = true
	opts.Report = filepath.Join(t.TempDir(), "rerun.json")
	if err := importUsers(opts); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Open(t.Context(), target)
	defer st.Close()
	got, _ := st.GetUserByUsername(t.Context(), "alice")
	if got.ID != u.ID || got.PasswordHash != h || got.Disabled {
		t.Fatal("rerun overwrote user")
	}
	db = openSQL(t, target)
	defer db.Close()
	assertCount(t, db, "sessions", 0)
}

func TestUsersIdentityConflictsRollback(t *testing.T) {
	for _, change := range []string{
		`UPDATE auth_user SET username='renamed' WHERE id=1`,
		`UPDATE auth_user SET id=99 WHERE id=1`,
	} {
		t.Run(change, func(t *testing.T) {
			source := djFixture(t, [][3]any{{"alice", 0, 1}})
			target := filepath.Join(t.TempDir(), "go.db")
			opts := userOptions(t, source, target)
			if err := importUsers(opts); err != nil {
				t.Fatal(err)
			}
			db := openSQL(t, source)
			mustExec(t, db, change)
			db.Close()
			opts.ExistingTarget = true
			opts.Report = filepath.Join(t.TempDir(), "report.json")
			if err := importUsers(opts); err == nil {
				t.Fatal("identity conflict accepted")
			}
			db = openSQL(t, target)
			defer db.Close()
			assertCount(t, db, "users", 1)
			if _, err := os.Stat(opts.Report); !os.IsNotExist(err) {
				t.Fatal("failure published report")
			}
		})
	}
}

func TestUsersFailureRollsBackEntireImport(t *testing.T) {
	source := djFixture(t, [][3]any{{"alice", 0, 1}, {"bob", 0, 1}})
	target := filepath.Join(t.TempDir(), "go.db")
	st, _ := store.Open(t.Context(), target)
	st.Close()
	db := openSQL(t, target)
	mustExec(t, db, `CREATE TRIGGER reject_bob BEFORE INSERT ON users WHEN NEW.username='bob' BEGIN SELECT RAISE(ABORT,'injected'); END`)
	db.Close()
	opts := userOptions(t, source, target)
	opts.ExistingTarget = true
	if err := importUsers(opts); err == nil {
		t.Fatal("failure accepted")
	}
	db = openSQL(t, target)
	defer db.Close()
	assertCount(t, db, "users", 0)
}

func TestUsersRefusesUnsafeTargetsAndBootstrapNames(t *testing.T) {
	source := djFixture(t, [][3]any{{"root", 1, 1}})
	for _, scenario := range []string{"existing", "same-source", "bootstrap-collision", "report-failure"} {
		t.Run(scenario, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "go.db")
			opts := userOptions(t, source, target)
			switch scenario {
			case "existing":
				st, _ := store.Open(t.Context(), target)
				st.Close()
			case "same-source":
				opts.DB = source
				opts.ExistingTarget = true
			case "bootstrap-collision":
				opts.BootstrapAdmin = "root"
			case "report-failure":
				opts.Report = filepath.Join(t.TempDir(), "missing", "report")
			}
			if err := importUsers(opts); err == nil {
				t.Fatal("unsafe import accepted")
			}
			if scenario == "bootstrap-collision" || scenario == "report-failure" {
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatal("failed new target was retained")
				}
			}
		})
	}
}

func TestJSONExportAndDuplicateIdentityValidation(t *testing.T) {
	for _, body := range []string{
		`[{"id":1,"username":"alice","is_superuser":false,"is_active":true}]`,
		`[{"id":1,"username":"alice"},{"id":1,"username":"bob"}]`,
		`[{"id":1,"username":"alice"},{"id":2,"username":"alice"}]`,
		`[{"id":1,"id":2,"username":"alice"}]`,
		`[{"id":0,"username":"alice"}]`,
	} {
		t.Run(body, func(t *testing.T) {
			source := mappingFixture(t, body)
			opts := userOptions(t, "", filepath.Join(t.TempDir(), "go.db"))
			opts.UsersJSON = source
			err := importUsers(opts)
			var rows []map[string]any
			_ = json.Unmarshal([]byte(body), &rows)
			valid := strings.Contains(body, `"is_active":true`)
			if valid && err != nil {
				t.Fatal(err)
			}
			if !valid && err == nil {
				t.Fatal("invalid export accepted")
			}
		})
	}
}

func openSQL(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}
func assertCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("%s count=%d want %d", table, n, want)
	}
}
