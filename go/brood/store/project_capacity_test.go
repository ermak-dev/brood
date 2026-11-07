package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"testing"
)

// FEATURE: Persist shard selectors and optional limits without losing ACL data (#69).
// SPEC: Nil differs from zero; replacement is atomic; recognized versions upgrade
// in one transaction, unknown or damaged versioned schemas never reconstruct data.
func projectLimit(value int64) *int64 { return &value }

func TestProjectCapacityRoundtrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capacity.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if err := s.CreateProject(ctx, Project{ID: "p", Name: "project"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProjectCapacity(ctx, "p")
	if err != nil || !reflect.DeepEqual(got, ProjectCapacity{}) {
		t.Fatalf("default=%+v, %v", got, err)
	}
	want := ProjectCapacity{WorkerSelector: map[string]string{"shard": "one"}, MaxSwarms: projectLimit(0), MaxActors: projectLimit(3), MaxCPUMillicores: projectLimit(4000), MaxMemoryBytes: projectLimit(math.MaxInt64)}
	if err := s.SetProjectCapacity(ctx, "p", want); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GetProjectCapacity(ctx, "p")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("roundtrip=%+v, %v; want %+v", got, err, want)
	}
	if err := s.SetProjectCapacity(ctx, "p", ProjectCapacity{}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetProjectCapacity(ctx, "p")
	if err != nil || !reflect.DeepEqual(got, ProjectCapacity{}) {
		t.Fatalf("replacement retained limits: %+v, %v", got, err)
	}
	if _, err := s.GetProjectCapacity(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing read: %v", err)
	}
	if err := s.SetProjectCapacity(ctx, "missing", want); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing write: %v", err)
	}
	if err := s.DeleteProject(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM project_capacity`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascade rows=%d, %v", count, err)
	}
}

func TestProjectCapacityRejectsInvalidWithoutMutation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateProject(ctx, Project{ID: "p", Name: "project"}); err != nil {
		t.Fatal(err)
	}
	want := ProjectCapacity{MaxActors: projectLimit(3)}
	if err := s.SetProjectCapacity(ctx, "p", want); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []ProjectCapacity{
		{MaxSwarms: projectLimit(-1)}, {MaxActors: projectLimit(-1)}, {MaxCPUMillicores: projectLimit(-1)}, {MaxMemoryBytes: projectLimit(-1)},
		{WorkerSelector: map[string]string{"": "one"}}, {WorkerSelector: map[string]string{"bad key": "one"}},
		{WorkerSelector: map[string]string{"shard": "bad value"}},
	} {
		if err := s.SetProjectCapacity(ctx, "p", invalid); err == nil {
			t.Fatalf("accepted %+v", invalid)
		}
		got, err := s.GetProjectCapacity(ctx, "p")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("failed write changed properties: %+v, %v", got, err)
		}
	}
	rollback := errors.New("rollback")
	if err := s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.SetProjectCapacity(ctx, "p", ProjectCapacity{MaxActors: projectLimit(0)}); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	got, err := s.GetProjectCapacity(ctx, "p")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("transaction leaked: %+v, %v", got, err)
	}
}

func TestProjectCapacityKnownSchemaUpgrade(t *testing.T) {
	for _, version := range []int{0, 1} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			path := legacyACLDatabase(t, false)
			if version == 1 {
				db, err := sql.Open("sqlite", dsn(path))
				if err != nil {
					t.Fatal(err)
				}
				for _, q := range []string{swarmMembersSchema, `INSERT INTO swarm_members VALUES ('sw','u1','owner')`, liveSwarmNameIndex, `PRAGMA user_version=1`} {
					if _, err := db.Exec(q); err != nil {
						db.Close()
						t.Fatal(err)
					}
				}
				db.Close()
			}
			s, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var current int
			if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil || current != 13 {
				t.Fatalf("version=%d, %v", current, err)
			}
			var hash, data string
			if err := s.db.QueryRow(`SELECT password_hash FROM users WHERE id='u1'`).Scan(&hash); err != nil || hash != "private-hash" {
				t.Fatalf("user changed: %q, %v", hash, err)
			}
			if err := s.db.QueryRow(`SELECT data FROM sessions WHERE token_hash='token-hash'`).Scan(&data); err != nil || data != "session-data" {
				t.Fatalf("session changed: %q, %v", data, err)
			}
			if role, err := s.GetSwarmRole(context.Background(), "sw", "u1"); err != nil || role != "owner" {
				t.Fatalf("owner changed: %q, %v", role, err)
			}
		})
	}
}

func TestProjectCapacityDamagedSchemaRefused(t *testing.T) {
	for _, q := range []string{`DROP TABLE project_capacity`, `PRAGMA user_version=99`, `DROP TABLE project_capacity; CREATE TABLE project_capacity (project_id TEXT)`} {
		t.Run(q, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "damaged.db")
			s, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(q); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s, err = Open(context.Background(), path)
			if err == nil {
				s.Close()
				t.Fatal("damaged/unknown schema accepted")
			}
		})
	}
	path := legacyACLDatabase(t, false)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE project_capacity (project_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(context.Background(), path)
	if err == nil {
		s.Close()
		t.Fatal("reserved legacy table accepted")
	}
	db, err = sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, count int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='swarm_members'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if version != 0 || count != 0 {
		t.Fatalf("partial upgrade leaked: version=%d, ACL tables=%d", version, count)
	}
}
