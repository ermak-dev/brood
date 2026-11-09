package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Recognize only the single current schema version.
// SPEC: A fresh database creates the full ACL schema; a legacy unversioned
// database is refused without changes, a damaged ACL database is not rebuilt.
// Миграций больше нет (#205): расширение прежней базовой схемы не выполняется.

// legacyACLDatabase — нерасширенная база до внедрения ACL: объекты есть,
// user_version равен нулю. Фикстура для проверки отказа без мутации.
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

func TestACLLegacyDatabaseRefusedWithoutChanges(t *testing.T) {
	for name, path := range map[string]string{
		"clean":       legacyACLDatabase(t, false),
		"conflicting": legacyACLDatabase(t, true),
	} {
		t.Run(name, func(t *testing.T) {
			db, err := sql.Open("sqlite", dsn(path))
			if err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, db)
			db.Close()
			if s, err := Open(context.Background(), path); err == nil {
				s.Close()
				t.Fatal("unversioned legacy database accepted")
			} else if !strings.Contains(err.Error(), "incompatible database schema") {
				t.Fatal(err)
			}
			db, err = sql.Open("sqlite", dsn(path))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var tables int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='swarm_members'`).Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if tables != 0 || !reflect.DeepEqual(before, schemaSnapshot(t, db)) {
				t.Fatalf("refused open mutated legacy database: ACL tables=%d", tables)
			}
		})
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
