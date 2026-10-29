package acceptance

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func testSessionExpiry(t *testing.T, bin binaries) {
	s := setUp(t, bin)
	s.f.cli("alice-state", "alice\n"+alicePassword+"\n", 0, "login", s.f.origin)
	expectCLIContains(t, s.f.cli("alice-state", "", 0, "whoami", s.f.origin), "alice")
	db, err := sql.Open("sqlite", filepath.Join(s.f.dir, "brood.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Меняется только срок собственных сессий disposable-установки, не часы сервера.
	result, err := db.Exec(`UPDATE sessions SET expires_at = ? WHERE user_id =
		(SELECT id FROM users WHERE username = ?)`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 2 {
		t.Fatalf("expired session count = %d, want browser and CLI sessions: %v", count, err)
	}
	s.alice.expect("GET", "/api/projects", nil, 401)
	s.f.cli("alice-state", "", 2, "whoami", s.f.origin)
	s.f.cli("alice-state", "", 2, "projects", "list", s.f.origin)
	expectProjects(t, s.bob, s.shared)
	s.admin.expect("GET", "/api/admin/users", nil, 200)
}
