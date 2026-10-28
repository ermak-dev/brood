package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// FEATURE: Add ACL to the precisely recognized current database without data loss.
// SPEC: Owners are backfilled once, grants survive reopen, conflicting names
// refuse the extension atomically, and a damaged ACL database is not rebuilt.
func legacyACLDatabase(t *testing.T, duplicate bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	data, err := os.ReadFile("testdata/schema-before-acl.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO users VALUES ('u1','alice',NULL,'private-hash',0,0,'now','now')`,
		`INSERT INTO users VALUES ('u2','bob',NULL,'other-hash',0,0,'now','now')`,
		`INSERT INTO projects VALUES ('p','project','now')`,
		`INSERT INTO project_members VALUES ('p','u1')`,
		`INSERT INTO swarms VALUES ('sw','p','u1','alpha','swarm-sw','uid','active','now','now','')`,
		`INSERT INTO sessions VALUES ('token-hash','u1','now',NULL,'session-data')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if duplicate {
		if _, err := db.Exec(`INSERT INTO swarms VALUES ('sw2','p','u2','alpha','swarm-sw2','uid2','active','now','now','')`); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestACLRecognizedDatabaseExtensionPreservesRecords(t *testing.T) {
	path := legacyACLDatabase(t, false)
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	role, err := s.GetSwarmRole(ctx, "sw", "u1")
	if err != nil || role != "owner" {
		t.Fatalf("backfilled role = %q, %v", role, err)
	}
	if err := s.SetSwarmMember(ctx, "sw", "u2", "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	role, err = s.GetSwarmRole(ctx, "sw", "u2")
	if err != nil || role != "viewer" {
		t.Fatalf("grant after reopen = %q, %v", role, err)
	}
	var hash, data string
	if err := s.db.QueryRow(`SELECT password_hash FROM users WHERE id='u1'`).Scan(&hash); err != nil || hash != "private-hash" {
		t.Fatalf("user changed during extension: %q, %v", hash, err)
	}
	if err := s.db.QueryRow(`SELECT data FROM sessions WHERE token_hash='token-hash'`).Scan(&data); err != nil || data != "session-data" {
		t.Fatalf("session changed during extension: %q, %v", data, err)
	}
	if _, err := s.db.Exec(`UPDATE swarm_members SET role='administrator' WHERE swarm_id='sw' AND user_id='u2'`); err == nil {
		t.Fatal("SQL role constraint accepted an unsupported role")
	}
}

func TestACLConflictingLegacyNamesRefuseWithoutChanges(t *testing.T) {
	path := legacyACLDatabase(t, true)
	s, err := Open(context.Background(), path)
	if err == nil {
		s.Close()
		t.Fatal("conflicting live names accepted during ACL extension")
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables, swarms int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='swarm_members'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM swarms`).Scan(&swarms); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || swarms != 2 {
		t.Fatalf("failed extension mutated data: ACL tables=%d swarms=%d", tables, swarms)
	}
}

func TestACLCurrentDatabaseRefusesMissingOwnerGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	seedSwarm(t, s, "sw", "p", "u1", "alpha")
	if _, err := s.db.Exec(`DELETE FROM swarm_members WHERE swarm_id='sw'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(context.Background(), path)
	if err == nil {
		s.Close()
		t.Fatal("current ACL database without an owner grant was accepted")
	}
}

func TestACLDamagedCurrentDatabaseDoesNotRecreateGrants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE swarm_members`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(context.Background(), path)
	if err == nil {
		s.Close()
		t.Fatal("damaged ACL schema was silently recreated")
	}
}
