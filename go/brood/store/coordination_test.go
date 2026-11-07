package store

// FEATURE: Durable dispatch leases and project landing tokens.
// SPEC: Atomic scoped claims, identity-bound release, durable request retry, no force stealing.
// IMPLEMENTATION: Schema 8 journal with unique active slots and exact-claim release.
// RESULT: IMPLEMENTED — concurrent claims, identity isolation, retries and reopen are verified.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func coordinationFixture(t *testing.T, s *Store) CoordinationClaim {
	t.Helper()
	managedFixture(t, s)
	for i, uid := range []string{"source", "target"} {
		if _, err := s.ReserveManagedActor(context.Background(), managedRequest(uid, "one", 101+i)); err != nil {
			t.Fatal(err)
		}
	}
	return CoordinationClaim{ProjectID: "one", SwarmID: "one", RequestID: "req", Source: CoordinationActor{UID: "source", Generation: 1}, Target: CoordinationActor{UID: "target", Generation: 1}, HumanID: "u"}
}

func TestCoordinationDispatchRetryRelease(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	c := coordinationFixture(t, s)
	got, err := s.ClaimDispatchLease(ctx, c)
	if err != nil || got != c {
		t.Fatalf("claim: %+v %v", got, err)
	}
	if got, err = s.ClaimDispatchLease(ctx, c); err != nil || got != c {
		t.Fatalf("retry: %+v %v", got, err)
	}
	other := c
	other.RequestID = "other"
	coordinationConflict(t, func() error { _, e := s.ClaimDispatchLease(ctx, other); return e }())
	mismatch := c
	mismatch.Source.Generation++
	coordinationConflict(t, s.ReleaseDispatchLease(ctx, mismatch))
	if err = s.ReleaseDispatchLease(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseDispatchLease(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimDispatchLease(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimDispatchLease(ctx, c); err != nil {
		t.Fatal(err)
	} // historical retry must not replace other
	coordinationConflict(t, func() error {
		third := c
		third.RequestID = "third"
		_, e := s.ClaimDispatchLease(ctx, third)
		return e
	}())
	changed := c
	changed.HumanID = "different"
	coordinationConflict(t, func() error { _, e := s.ClaimDispatchLease(ctx, changed); return e }())
}
func coordinationConflict(t *testing.T, err error) {
	t.Helper()
	var conflict *CoordinationConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("expected typed conflict, got %v", err)
	}
}

func TestCoordinationConcurrentLandingAndDispatch(t *testing.T) {
	for _, kind := range []string{"dispatch", "landing"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			c := coordinationFixture(t, s)
			var wg sync.WaitGroup
			results := make(chan error, 20)
			for i := range 20 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					q := c
					q.RequestID = fmt.Sprint(i)
					var err error
					if kind == "dispatch" {
						_, err = s.ClaimDispatchLease(ctx, q)
					} else {
						q.Target = CoordinationActor{}
						_, err = s.TakeLandingToken(ctx, q)
					}
					results <- err
				}(i)
			}
			wg.Wait()
			close(results)
			success := 0
			for err := range results {
				if err == nil {
					success++
				} else {
					coordinationConflict(t, err)
				}
			}
			if success != 1 {
				t.Fatalf("winners %d", success)
			}
		})
	}
}

func TestCoordinationLandingReopenAndScope(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "coord.sqlite")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	c := coordinationFixture(t, s)
	c.Target = CoordinationActor{}
	if _, err = s.TakeLandingToken(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got, err := s.TakeLandingToken(ctx, c); err != nil || got != c {
		t.Fatalf("landing retry %+v %v", got, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.ShowLandingToken(ctx, "one")
	if err != nil || got != c {
		t.Fatalf("reopen %+v %v", got, err)
	}
	wrong := c
	wrong.SwarmID = "two"
	coordinationConflict(t, s.GiveLandingToken(ctx, wrong))
	if err = s.GiveLandingToken(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TakeLandingToken(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ShowLandingToken(ctx, "one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("show released: %v", err)
	}
	// A second swarm of the same project has its own dispatch targets, but shares landing.
	if err = s.CreateSwarm(ctx, Swarm{ID: "sibling", ProjectID: "one", OwnerID: "u", Name: "sibling", AtespaceName: "sibling", AtespaceUID: ptr("native-sibling"), State: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE managed_pools SET max_actors=4,cpu_millicores=4000,memory_bytes=4096,disk_bytes=8192`); err != nil {
		t.Fatal(err)
	}
	for i, uid := range []string{"sibling-source", "sibling-target"} {
		if _, err = s.ReserveManagedActor(ctx, managedRequest(uid, "sibling", 201+i)); err != nil {
			t.Fatal(err)
		}
	}
	d := c
	d.RequestID = "sibling"
	d.SwarmID = "sibling"
	d.Source.UID = "sibling-source"
	d.Target = CoordinationActor{UID: "sibling-target", Generation: 1}
	c.RequestID = "dispatch"
	c.Target = CoordinationActor{UID: "target", Generation: 1}
	if _, err = s.ClaimDispatchLease(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimDispatchLease(ctx, d); err != nil {
		t.Fatal(err)
	}
	bad := c
	bad.RequestID = "cross"
	bad.Target = d.Target
	coordinationConflict(t, func() error { _, e := s.ClaimDispatchLease(ctx, bad); return e }())
	c.Target = CoordinationActor{}
	c.RequestID = "landing-new"
	if _, err = s.TakeLandingToken(ctx, c); err != nil {
		t.Fatal(err)
	}
	d.Target = CoordinationActor{}
	coordinationConflict(t, func() error { _, e := s.TakeLandingToken(ctx, d); return e }())
}

func TestCoordinationIdentityValidation(t *testing.T) {
	for _, mutation := range []string{"generation", "deleted", "revoked", "project", "empty", "human"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			c := coordinationFixture(t, s)
			var query string
			switch mutation {
			case "generation":
				c.Source.Generation++
			case "deleted":
				query = `UPDATE managed_actors SET phase='deleted' WHERE uid='target'`
			case "revoked":
				query = `UPDATE managed_actors SET desired='deleted' WHERE uid='source'`
			case "project":
				c.ProjectID = "two"
			case "empty":
				c.RequestID = ""
			case "human":
				query = `UPDATE users SET disabled=1 WHERE id='u'`
			}
			if query != "" {
				if _, err := s.db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ClaimDispatchLease(ctx, c); err == nil {
				t.Fatal("invalid claim accepted")
			}
		})
	}
}

func TestCoordinationDispatchReopenAndStaleOwner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dispatch.sqlite")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	c := coordinationFixture(t, s)
	if _, err = s.ClaimDispatchLease(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE managed_actors SET incarnation=2 WHERE uid='source'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.ShowDispatchLease(ctx, "one", "one", "target")
	if err != nil || got != c {
		t.Fatalf("show stale lease %+v %v", got, err)
	}
	if _, err = s.ShowDispatchLease(ctx, "two", "one", "target"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong project: %v", err)
	}
	if _, err = s.ShowDispatchLease(ctx, "one", "two", "target"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong swarm: %v", err)
	}
	fresh := c
	fresh.Source.Generation = 2
	fresh.RequestID = "fresh"
	coordinationConflict(t, func() error { _, e := s.ClaimDispatchLease(ctx, fresh); return e }())
	if err = s.ReleaseDispatchLease(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimDispatchLease(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseDispatchLease(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err = s.ShowDispatchLease(ctx, "one", "one", "target")
	if err != nil || got != fresh {
		t.Fatalf("old release changed successor %+v %v", got, err)
	}
}

func TestCoordinationRecreatedNameDoesNotInheritToken(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	c := coordinationFixture(t, s)
	c.Target = CoordinationActor{}
	if _, err := s.TakeLandingToken(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE managed_actors SET phase='deleted',desired='deleted' WHERE uid='source'`); err != nil {
		t.Fatal(err)
	}
	replacement := managedRequest("new-source", "one", 103)
	replacement.Name = "source"
	if _, err := s.ReserveManagedActor(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	next := c
	next.Source.UID = "new-source"
	next.RequestID = "next"
	coordinationConflict(t, s.GiveLandingToken(ctx, next))
	coordinationConflict(t, func() error { _, e := s.TakeLandingToken(ctx, next); return e }())
	if got, err := s.ShowLandingToken(ctx, "one"); err != nil || got != c {
		t.Fatalf("replacement changed token %+v %v", got, err)
	}
	if err := s.GiveLandingToken(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeLandingToken(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := s.GiveLandingToken(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ShowLandingToken(ctx, "one"); err != nil || got != next {
		t.Fatalf("old release changed token %+v %v", got, err)
	}
}

func TestCoordinationIndependentConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	a, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := coordinationFixture(t, a)
	b, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	results := make(chan error, 2)
	start := make(chan struct{})
	for i, s := range []*Store{a, b} {
		go func(i int, s *Store) {
			<-start
			q := c
			q.RequestID = fmt.Sprint(i)
			_, err := s.ClaimDispatchLease(ctx, q)
			results <- err
		}(i, s)
	}
	close(start)
	wins := 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else {
			coordinationConflict(t, err)
		}
	}
	if wins != 1 {
		t.Fatalf("independent connection winners: %d", wins)
	}
}

func TestCoordinationUnversionedObjectsRollback(t *testing.T) {
	ctx := context.Background()
	path := lifecycleSchema3(t)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE coordination_claims (evidence TEXT); INSERT INTO coordination_claims VALUES ('keep')`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, db)
	db.Close()
	if s, err := Open(ctx, path); err == nil {
		s.Close()
		t.Fatal("unversioned coordination accepted")
	}
	db, err = sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	var evidence string
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 3 {
		t.Fatalf("version changed %d %v", version, err)
	}
	if !reflect.DeepEqual(before, schemaSnapshot(t, db)) {
		t.Fatal("rejected migration changed schema")
	}
	if err = db.QueryRow(`SELECT evidence FROM coordination_claims`).Scan(&evidence); err != nil || evidence != "keep" {
		t.Fatalf("evidence changed %q %v", evidence, err)
	}
}

func TestCoordinationMigrationAndValidation(t *testing.T) {
	ctx := context.Background()
	path := lifecycleSchema3(t)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`ALTER TABLE managed_actors DROP COLUMN origin; DROP TRIGGER pipeline_preparations_actor_fence; DROP TRIGGER pipeline_preparations_actor_no_replace; DROP TRIGGER pipeline_preparations_step_no_replace; DROP TRIGGER pipeline_preparations_step_update_fence; DROP TRIGGER pipeline_preparations_run_no_delete; DROP TABLE pipeline_preparations; DROP TRIGGER pipeline_runs_no_replace; DROP TABLE pipeline_sources; DROP TABLE workflow_revision_counters; DROP TABLE workflow_definitions; DROP TABLE pipeline_steps; DROP TABLE pipeline_runs; DROP TABLE project_repositories; DROP TABLE project_secret_files; DROP TABLE project_secret_vars; DROP TABLE project_secret_revisions; DROP TABLE coordination_claims; DROP TABLE command_project_details; PRAGMA user_version=7`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 16 {
		t.Fatalf("version %d: %v", version, err)
	}
	if _, err = s.db.Exec(`ALTER TABLE coordination_claims ADD COLUMN unexpected TEXT`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if broken, err := Open(ctx, path); err == nil {
		broken.Close()
		t.Fatal("accepted malformed coordination schema")
	}
}
