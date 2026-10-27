// Регрессии границ источника, назначения и реального HTTP-сброса пароля.
package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

func TestSourceIsReadOnlyAndDjangoSessionsAreNotCopied(t *testing.T) {
	source := djFixture(t, [][3]any{{"alice", 0, 1}})
	db := openSQL(t, source)
	mustExec(t, db, `CREATE TABLE django_session(session_key TEXT,session_data TEXT)`)
	mustExec(t, db, `INSERT INTO django_session VALUES('django-session','sensitive')`)
	db.Close()
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "go.db")
	if err := importUsers(userOptions(t, source, target)); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("source bytes changed")
	}
	db = openSQL(t, target)
	defer db.Close()
	assertCount(t, db, "sessions", 0)
}

func TestSourceMustNotAliasCompatibleTarget(t *testing.T) {
	source := djFixture(t, [][3]any{{"alice", 0, 1}})
	target := filepath.Join(t.TempDir(), "go.db")
	if err := importUsers(userOptions(t, source, target)); err != nil {
		t.Fatal(err)
	}
	db := openSQL(t, target)
	mustExec(t, db, `CREATE TABLE auth_user(id INTEGER,username TEXT,is_superuser INTEGER,is_active INTEGER)`)
	mustExec(t, db, `INSERT INTO auth_user VALUES(1,'alice',0,1)`)
	db.Close()
	opts := userOptions(t, target, target)
	opts.ExistingTarget = true
	if err := importUsers(opts); err == nil {
		t.Fatal("source and target may not be the same file")
	}
}

func TestDifferentSourceAndExistingBootstrapConflict(t *testing.T) {
	source := djFixture(t, [][3]any{{"alice", 0, 1}})
	target := filepath.Join(t.TempDir(), "go.db")
	opts := userOptions(t, source, target)
	if err := importUsers(opts); err != nil {
		t.Fatal(err)
	}
	opts.ExistingTarget = true
	opts.Report = filepath.Join(t.TempDir(), "report.json")
	opts.SourceID = "other-installation"
	if err := importUsers(opts); err == nil {
		t.Fatal("same username from another source accepted")
	}
	opts.SourceID = "installation-one"
	st, _ := store.Open(t.Context(), target)
	if err := st.CreateUser(t.Context(), store.User{ID: "disabled-admin", Username: opts.BootstrapAdmin, PasswordHash: "unused", IsAdmin: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := importUsers(opts); err == nil {
		t.Fatal("disabled bootstrap account accepted")
	}
	opts.BootstrapAdmin = "another-recovery-admin"
	if err := importUsers(opts); err != nil {
		t.Fatal(err)
	}
}

func TestReportFailureRollsBackExistingTargetAndSessions(t *testing.T) {
	source := djFixture(t, [][3]any{{"alice", 0, 1}})
	target := goDB(t, t.TempDir())
	db := openSQL(t, target)
	mustExec(t, db, `INSERT INTO sessions(token_hash,user_id,created_at) VALUES('keep','alice-id','now')`)
	db.Close()
	// Имя в источнике отличается от существующих пользователей назначения.
	db = openSQL(t, source)
	mustExec(t, db, `UPDATE auth_user SET username='new-user'`)
	db.Close()
	opts := userOptions(t, source, target)
	opts.ExistingTarget = true
	opts.Report = filepath.Join(t.TempDir(), "missing", "report.json")
	if err := importUsers(opts); err == nil {
		t.Fatal("report write failure accepted")
	}
	db = openSQL(t, target)
	defer db.Close()
	assertCount(t, db, "users", 2)
	assertCount(t, db, "sessions", 1)
}

func TestImportedAccountCanUseRealAdminResetAPI(t *testing.T) {
	source := djFixture(t, [][3]any{{"alice", 0, 1}})
	target := filepath.Join(t.TempDir(), "go.db")
	opts := userOptions(t, source, target)
	if err := importUsers(opts); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, err := auth.HashPassword("admin-password", auth.DefaultArgon2())
	if err != nil {
		t.Fatal(err)
	}
	if created, err := st.BootstrapAdmin(t.Context(), opts.BootstrapAdmin, hash); err != nil || !created {
		t.Fatalf("bootstrap: %v %v", created, err)
	}
	svc := auth.NewService(st, auth.Config{}, nil)
	mux := http.NewServeMux()
	svc.Mount(mux)
	handler := svc.Middleware(mux)
	cookies := map[string]*http.Cookie{}
	request := func(method, path, body string, want int) {
		t.Helper()
		req := httptest.NewRequest(method, "http://migration.test"+path, strings.NewReader(body))
		req.Header.Set("Origin", "http://migration.test")
		req.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		if cookie := cookies[auth.CookieCSRF]; cookie != nil {
			req.Header.Set(auth.HeaderCSRF, cookie.Value)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		for _, cookie := range rec.Result().Cookies() {
			if cookie.Value == "" || cookie.MaxAge < 0 {
				delete(cookies, cookie.Name)
			} else {
				cookies[cookie.Name] = cookie
			}
		}
	}
	request("GET", "/api/auth/session", "", 200)
	request("POST", "/api/auth/login", `{"username":"recovery-admin","password":"admin-password"}`, http.StatusNoContent)
	request("POST", "/api/admin/users/alice/password-reset", `{"new_password":"approved-password"}`, 204)
	u, err := st.GetUserByUsername(t.Context(), "alice")
	if err != nil || !u.Disabled {
		t.Fatal("password reset must not enable account")
	}
	request("POST", "/api/admin/users/alice/enable", `{}`, 204)
	cookies = map[string]*http.Cookie{}
	request("GET", "/api/auth/session", "", 200)
	request("POST", "/api/auth/login", `{"username":"alice","password":"approved-password"}`, http.StatusNoContent)
}

func TestCommandHelpAndStrictArguments(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"users", "--help"}, {"swarms", "--help"}} {
		var out bytes.Buffer
		if run(args, &out) != 0 || !strings.Contains(out.String(), "Offline copies only") {
			t.Fatal("missing usable help")
		}
	}
	for _, args := range [][]string{{"users", "positional"}, {"users", "--db", "--source-id"}, {"users", "--db", "a", "--db", "b"}, {"swarms", "--existing-target", "no"}} {
		var out bytes.Buffer
		if run(args, &out) == 0 {
			t.Fatal("invalid arguments accepted")
		}
	}
}
