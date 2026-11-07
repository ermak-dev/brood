package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testOrigin = "https://git.example.com/group/repo.git"

// FEATURE: Durable actor origin and project repository (#146).
// SPEC: Schema 10 adds managed_actors.origin and project_repositories without
// resetting data; a v9 database upgrades in place, a damaged or unversioned
// v10 schema is refused without mutation. Origins are stored canonical only.
// IMPLEMENTATION: One additive ALTER/CREATE inside the opening transaction.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestOriginSchemaUpgradeFromV9PreservesData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "origin.sqlite3")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	managedFixture(t, s)
	if _, err := s.ReserveManagedActor(ctx, managedRequest("legacy", "one", 101)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProjectSecretVars(ctx, "one", map[string]string{"APP_TOKEN": "keep-value"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	downgradeOriginSchema(t, path)
	for range 2 {
		s, err = Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 15 {
			t.Fatalf("version=%d err=%v", version, err)
		}
		a, err := s.GetManagedActor(ctx, "legacy")
		if err != nil || a.Origin != "" || a.CTID != 101 || a.Name != "legacy" {
			t.Fatalf("upgrade lost actor: %+v %v", a, err)
		}
		repository, err := s.ProjectRepository(ctx, "one")
		if err != nil || repository != "" {
			t.Fatalf("upgrade invented a repository: %q %v", repository, err)
		}
		bundle, err := s.ReadProjectSecretBundle(ctx, "one")
		if err != nil || bundle.Vars["APP_TOKEN"] != "keep-value" {
			t.Fatalf("upgrade lost project secret: %v", err)
		}
		s.Close()
	}
}

// downgradeOriginSchema turns a current database into the exact schema 9.
func downgradeOriginSchema(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE managed_actors DROP COLUMN origin; DROP TRIGGER pipeline_runs_no_replace; DROP TABLE pipeline_sources; DROP TABLE workflow_revision_counters; DROP TABLE workflow_definitions; DROP TABLE pipeline_steps; DROP TABLE pipeline_runs; DROP TABLE project_repositories; PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
}

func TestOriginSchemaRefusesUnversionedObjects(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "origin.sqlite3")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	downgradeOriginSchema(t, path)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE project_repositories (project_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, db)
	if st, err := Open(ctx, path); err == nil {
		st.Close()
		t.Fatal("unversioned repository table accepted")
	} else if !strings.Contains(err.Error(), "incompatible database schema") {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 9 || !reflect.DeepEqual(before, schemaSnapshot(t, db)) {
		t.Fatalf("refused upgrade mutated schema: version=%d err=%v", version, err)
	}
}

func TestOriginSchemaDamageRefusedWithoutMutation(t *testing.T) {
	for _, change := range []string{
		`ALTER TABLE managed_actors DROP COLUMN origin`,
		`DROP TABLE project_repositories`,
		`DROP TABLE project_repositories; CREATE TABLE project_repositories (project_id TEXT PRIMARY KEY, origin TEXT)`,
		`PRAGMA user_version=16`,
	} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db.sqlite3")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(change); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, s.db)
			s.Close()
			if st, err := Open(ctx, path); err == nil {
				st.Close()
				t.Fatal("damaged origin schema accepted")
			} else if !strings.Contains(err.Error(), "incompatible database schema") {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", dsn(path))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if !reflect.DeepEqual(before, schemaSnapshot(t, db)) {
				t.Fatal("rejected schema mutated")
			}
		})
	}
}

func TestManagedActorOriginIsCanonicalAndDurable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	for _, origin := range []string{"https://Git.example.com/group/repo.git", "http://git.example.com/repo", "https://user:secret@git.example.com/repo", "https://git.example.com/repo/"} {
		request := managedRequest("bad", "one", 101)
		request.Origin = origin
		if _, err := s.ReserveManagedActor(ctx, request); err == nil {
			t.Fatalf("noncanonical origin stored: %q", origin)
		}
	}
	request := managedRequest("cloned", "one", 102)
	request.Origin = testOrigin
	a, err := s.ReserveManagedActor(ctx, request)
	if err != nil || a.Origin != testOrigin {
		t.Fatalf("reserve: %+v %v", a, err)
	}
	got, err := s.GetManagedActor(ctx, "cloned")
	if err != nil || got.Origin != testOrigin {
		t.Fatalf("origin not durable: %+v %v", got, err)
	}
	got.Origin = "https://git.example.com/other.git"
	got.Phase = "error"
	if _, err := s.UpdateManagedActor(ctx, got, got.Revision); !errors.Is(err, ErrManagedBinding) {
		t.Fatalf("origin changed by observation update: %v", err)
	}
	plain, err := s.ReserveManagedActor(ctx, managedRequest("plain", "two", 103))
	if err != nil || plain.Origin != "" {
		t.Fatalf("empty origin: %+v %v", plain, err)
	}
}

func TestProjectRepository(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "project")
	if got, err := s.ProjectRepository(ctx, "p"); err != nil || got != "" {
		t.Fatalf("default repository: %q %v", got, err)
	}
	if err := s.SetProjectRepository(ctx, "p", testOrigin); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ProjectRepository(ctx, "p"); err != nil || got != testOrigin {
		t.Fatalf("repository: %q %v", got, err)
	}
	for _, origin := range []string{"https://Git.example.com/group/repo.git", "http://git.example.com/repo", "https://user:secret@git.example.com/repo", " "} {
		if err := s.SetProjectRepository(ctx, "p", origin); err == nil {
			t.Fatalf("noncanonical repository stored: %q", origin)
		}
	}
	if got, _ := s.ProjectRepository(ctx, "p"); got != testOrigin {
		t.Fatal("refused write changed the repository")
	}
	if err := s.SetProjectRepository(ctx, "missing", testOrigin); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if _, err := s.ProjectRepository(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project read: %v", err)
	}
	if err := s.SetProjectRepository(ctx, "p", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ProjectRepository(ctx, "p"); err != nil || got != "" {
		t.Fatalf("cleared repository: %q %v", got, err)
	}
	if err := s.SetProjectRepository(ctx, "p", testOrigin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM projects WHERE id='p'`); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM project_repositories`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("project deletion kept repository: %d %v", rows, err)
	}
}
