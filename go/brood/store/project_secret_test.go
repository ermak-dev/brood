package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/projectsecret"
)

// FEATURE: Private project secret storage and validated schema 9 (#134).
// SPEC: Atomic bounded batches, monotonic revisions, metadata-only listing, project FK and durable reopen.
// IMPLEMENTATION: Validated additive schema 9, immediate transactions and per-batch savepoints.
// RESULT: IMPLEMENTED — roundtrip, concurrent writers, bounded rollback and migration checks pass.
// STATUS: DONE
func TestProjectSecretRoundtrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secret.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	seedProject(t, s, "p", "p")
	seedProject(t, s, "q", "q")
	meta, err := s.GetProjectSecretMetadata(ctx, "p")
	if err != nil || meta.Revision != 0 || len(meta.Files) != 0 || len(meta.Vars) != 0 {
		t.Fatal("empty defaults", err)
	}
	files := []projectsecret.File{{Name: "z", Data: []byte{0, 255, 1}}, {Name: ".env", Data: []byte("fake=value\n")}}
	if err = s.PutProjectSecretFiles(ctx, "p", files); err != nil {
		t.Fatal(err)
	}
	if err = s.PutProjectSecretVars(ctx, "p", map[string]string{"TOKEN": " 'fake = value' ", "EMPTY": ""}); err != nil {
		t.Fatal(err)
	}
	if err = s.PutProjectSecretFiles(ctx, "p", []projectsecret.File{{Name: "z", Data: []byte("replacement")}}); err != nil {
		t.Fatal(err)
	}
	meta, err = s.GetProjectSecretMetadata(ctx, "p")
	want := projectsecret.Metadata{Revision: 3, Files: []projectsecret.FileInfo{{Name: ".env", Size: 11}, {Name: "z", Size: 11}}, Vars: []string{"EMPTY", "TOKEN"}}
	if err != nil || !reflect.DeepEqual(meta, want) {
		t.Fatal("metadata differs", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := s.ReadProjectSecretBundle(ctx, "p")
	if err != nil || bundle.Revision != 3 || string(bundle.Files[1].Data) != "replacement" || bundle.Vars["TOKEN"] != " 'fake = value' " {
		t.Fatal("roundtrip differs", err)
	}
	if err = s.RemoveProjectSecretFiles(ctx, "p", []string{"z", "missing"}); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveProjectSecretVars(ctx, "p", []string{"TOKEN", "MISSING"}); err != nil {
		t.Fatal(err)
	}
	meta, err = s.GetProjectSecretMetadata(ctx, "p")
	if err != nil || meta.Revision != 5 || len(meta.Files) != 1 || !reflect.DeepEqual(meta.Vars, []string{"EMPTY"}) {
		t.Fatal("removal failed", err)
	}
	other, err := s.ReadProjectSecretBundle(ctx, "q")
	if err != nil || other.Revision != 0 || len(other.Files)+len(other.Vars) != 0 {
		t.Fatal("project isolation", err)
	}
	if err = s.DeleteProject(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"project_secret_revisions", "project_secret_files", "project_secret_vars"} {
		var n int
		if err = s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal("cascade failed", err)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("database is not private", err)
	}
}

func TestProjectSecretAtomicBounds(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "p")
	for _, call := range []func() error{
		func() error { _, e := s.GetProjectSecretMetadata(ctx, "missing"); return e },
		func() error { _, e := s.ReadProjectSecretBundle(ctx, "missing"); return e },
		func() error { return s.PutProjectSecretFiles(ctx, "missing", nil) },
		func() error { return s.PutProjectSecretVars(ctx, "missing", nil) },
		func() error { return s.RemoveProjectSecretFiles(ctx, "missing", nil) },
		func() error { return s.RemoveProjectSecretVars(ctx, "missing", nil) },
	} {
		if !errors.Is(call(), ErrNotFound) {
			t.Fatal("missing project not refused")
		}
	}
	large := []projectsecret.File{}
	for i := 0; i < 4; i++ {
		large = append(large, projectsecret.File{Name: fmt.Sprintf("f%d", i), Data: make([]byte, 512*1024)})
	}
	if err := s.PutProjectSecretFiles(ctx, "p", large); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ReadProjectSecretBundle(ctx, "p")
	for _, batch := range [][]projectsecret.File{
		{{Name: "new", Data: []byte("x")}},
		{{Name: "f0", Data: []byte("changed")}, {Name: "../bad"}},
		{{Name: "f0/child"}},
	} {
		if s.PutProjectSecretFiles(ctx, "p", batch) == nil {
			t.Fatal("invalid batch accepted")
		}
	}
	if s.RemoveProjectSecretFiles(ctx, "p", []string{"f0", "../bad"}) == nil {
		t.Fatal("invalid remove accepted")
	}
	after, _ := s.ReadProjectSecretBundle(ctx, "p")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("partial file update")
	}
	vars := map[string]string{"A": strings.Repeat("x", 16*1024), "B": strings.Repeat("x", 16*1024), "C": strings.Repeat("x", 16*1024)}
	if err := s.PutProjectSecretVars(ctx, "p", vars); err != nil {
		t.Fatal(err)
	}
	before, _ = s.ReadProjectSecretBundle(ctx, "p")
	if s.PutProjectSecretVars(ctx, "p", map[string]string{"D": strings.Repeat("x", 16*1024)}) == nil {
		t.Fatal("aggregate environment accepted")
	}
	if s.PutProjectSecretVars(ctx, "p", map[string]string{"A": "new", "BROOD_OVERRIDE": "fake"}) == nil {
		t.Fatal("invalid var accepted")
	}
	if s.RemoveProjectSecretVars(ctx, "p", []string{"A", "BAD-NAME"}) == nil {
		t.Fatal("invalid remove accepted")
	}
	after, _ = s.ReadProjectSecretBundle(ctx, "p")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("partial var update")
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if e := s.RemoveProjectSecretVars(ctx, "p", []string{"A"}); e != nil {
			return e
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	after, _ = s.ReadProjectSecretBundle(ctx, "p")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("outer transaction rollback lost")
	}
}

func TestProjectSecretConcurrentUpdates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.db")
	s, e := Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	seedProject(t, s, "p", "p")
	second, e := Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			writer := s
			if i%2 == 0 {
				writer = second
			}
			if err := writer.PutProjectSecretVars(ctx, "p", map[string]string{fmt.Sprintf("V%d", i): "fake"}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	b, e := s.ReadProjectSecretBundle(ctx, "p")
	if e != nil || b.Revision != 32 || len(b.Vars) != 32 {
		t.Fatal("lost concurrent update", e)
	}
}

// SPEC: A failed database batch stays atomic even if an outer transaction handles its error.
func TestProjectSecretNestedBatchFailure(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "p")
	if _, err := s.db.Exec(`CREATE TRIGGER secret_test_failure BEFORE INSERT ON project_secret_files WHEN NEW.name='second' BEGIN SELECT RAISE(ABORT,'SENTINEL'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.WithTx(ctx, func(ctx context.Context) error {
		err := s.PutProjectSecretFiles(ctx, "p", []projectsecret.File{{Name: "first", Data: []byte("fake")}, {Name: "second", Data: []byte("fake")}})
		if err == nil || strings.Contains(err.Error(), "SENTINEL") {
			t.Fatal("batch failure missing or diagnostic leaked")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.ReadProjectSecretBundle(ctx, "p")
	if err != nil || b.Revision != 0 || len(b.Files) != 0 {
		t.Fatal("failed nested batch left partial data", err)
	}
}

func TestProjectSecretSchemaMigration(t *testing.T) {
	ctx := context.Background()
	for _, damage := range []string{"", "reserved", "old-schema", "versioned"} {
		t.Run(damage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "migration.db")
			s, e := Open(ctx, path)
			if e != nil {
				t.Fatal(e)
			}
			seedProject(t, s, "p", "p")
			if damage != "versioned" {
				_, e = s.db.Exec(`ALTER TABLE managed_actors DROP COLUMN origin; DROP TABLE project_repositories; DROP TABLE project_secret_files; DROP TABLE project_secret_vars; DROP TABLE project_secret_revisions; PRAGMA user_version=8`)
				if e != nil {
					t.Fatal(e)
				}
			}
			switch damage {
			case "reserved":
				_, e = s.db.Exec(`CREATE TABLE project_secret_files (bad TEXT)`)
			case "old-schema":
				_, e = s.db.Exec(`DROP TABLE coordination_claims; PRAGMA user_version=7; CREATE TABLE project_secret_files (bad TEXT)`)
			case "versioned":
				_, e = s.db.Exec(`DROP TABLE project_secret_vars; CREATE TABLE project_secret_vars (bad TEXT)`)
			}
			if e != nil {
				t.Fatal(e)
			}
			before := schemaSnapshot(t, s.db)
			s.Close()
			reopened, e := Open(ctx, path)
			if damage == "" {
				if e != nil {
					t.Fatal(e)
				}
				defer reopened.Close()
				var v int
				reopened.db.QueryRow(`PRAGMA user_version`).Scan(&v)
				if v != 10 {
					t.Fatal("schema version", v)
				}
				if _, e = reopened.GetProject(ctx, "p"); e != nil {
					t.Fatal(e)
				}
				return
			}
			if e == nil {
				reopened.Close()
				t.Fatal("damaged schema accepted")
			}
			raw, e := sql.Open("sqlite", dsn(path))
			if e != nil {
				t.Fatal(e)
			}
			defer raw.Close()
			if !reflect.DeepEqual(before, schemaSnapshot(t, raw)) {
				t.Fatal("failed migration changed schema")
			}
			var v int
			raw.QueryRow(`PRAGMA user_version`).Scan(&v)
			want := 8
			if damage == "old-schema" {
				want = 7
			}
			if damage == "versioned" {
				want = 10
			}
			if v != want {
				t.Fatal("failed migration changed version")
			}
		})
	}
}
