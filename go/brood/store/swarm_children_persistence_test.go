package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestSwarmChildIndependentConnections(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	var file, dbname string
	var sequence int
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &dbname, &file))
	second, err := Open(ctx, file)
	mustNil(t, err)
	defer second.Close()
	type response struct {
		child SwarmChildTask
		err   error
	}
	results := make(chan response, 2)
	var wg sync.WaitGroup
	for _, handle := range []*Store{s, second} {
		wg.Add(1)
		go func(handle *Store) {
			defer wg.Done()
			c, err := handle.ReserveSwarmChild(ctx, p, "same", "durable payload", e, childAllow)
			results <- response{c, err}
		}(handle)
	}
	wg.Wait()
	first, other := <-results, <-results
	mustNil(t, first.err)
	mustNil(t, other.err)
	if !reflect.DeepEqual(first.child, other.child) {
		t.Fatalf("independent handles reserved different children: %+v %+v", first, other)
	}
	for _, handle := range []*Store{s, second} {
		wg.Add(1)
		go func(handle *Store) {
			defer wg.Done()
			c, err := handle.StartSwarmChild(ctx, p, "same", e, 1, childAllow)
			results <- response{c, err}
		}(handle)
	}
	wg.Wait()
	claims := 0
	for range 2 {
		r := <-results
		if r.err == nil {
			claims++
		} else if !errors.Is(r.err, ErrSwarmChildConflict) {
			t.Fatal(r.err)
		}
	}
	if claims != 1 {
		t.Fatalf("independent handles accepted %d claims", claims)
	}
	for i, handle := range []*Store{s, second} {
		wg.Add(1)
		go func(i int, handle *Store) {
			defer wg.Done()
			outcome := "success"
			if i == 1 {
				outcome = "fail"
			}
			c, err := handle.CompleteSwarmChild(ctx, p, "same", e, 2, outcome, "report", childAllow)
			results <- response{c, err}
		}(i, handle)
	}
	wg.Wait()
	completions := 0
	for range 2 {
		r := <-results
		if r.err == nil {
			completions++
		} else if !errors.Is(r.err, ErrSwarmChildConflict) {
			t.Fatal(r.err)
		}
	}
	if completions != 1 {
		t.Fatalf("concurrent different outcomes accepted %d completions", completions)
	}
}

func TestSwarmChildPolicyRechecksRevokedSession(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	mustNil(t, s.CreateSession(ctx, Session{TokenHash: "child-authority", UserID: "u"}))
	policy := func(ctx context.Context, a SwarmChildAuthorization) error {
		session, err := s.GetSession(ctx, "child-authority")
		if err != nil || session.UserID != "u" {
			return ErrSwarmChildDenied
		}
		if a.Parent.Principal.SwarmID != "one" {
			return ErrSwarmChildDenied
		}
		_, err = s.requireSwarmManager(ctx, "one", session.UserID)
		return err
	}
	c, err := s.ReserveSwarmChild(ctx, p, "same", "task", e, policy)
	mustNil(t, err)
	mustNil(t, s.DeleteSession(ctx, "child-authority"))
	before := dumpTables(t, s, "swarm_child_tasks", "swarm_child_joins")
	if _, err := s.ReserveSwarmChild(ctx, p, "same", "task", e, policy); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatalf("exact retry with revoked session: %v", err)
	}
	if _, err := s.StartSwarmChild(ctx, p, "same", e, c.Revision, policy); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatalf("claim with revoked session: %v", err)
	}
	if _, err := s.GetSwarmChildJoin(ctx, p, policy); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatalf("read with revoked session: %v", err)
	}
	if after := dumpTables(t, s, "swarm_child_tasks", "swarm_child_joins"); after != before {
		t.Fatal("revoked authority changed children")
	}
}

func TestMigrateV34ChildStoragePreservesEveryRow(t *testing.T) {
	ctx := context.Background()
	path := loadSchemaFixture(t, 34)
	raw := rawDatabase(t, path)
	before := databaseRows(t, raw)
	raw.Close()
	s, err := Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	before["swarm_child_joins"] = nil
	before["swarm_child_tasks"] = nil
	before["actor_interventions"] = nil
	before["actor_step_interruptions"] = nil
	if after := databaseRows(t, s.db); !reflect.DeepEqual(after, before) {
		t.Fatal("v34→v35 changed existing rows or created children")
	}
	fresh := newTestStore(t)
	if !reflect.DeepEqual(schemaObjects(t, s.db), schemaObjects(t, fresh.db)) {
		t.Fatal("v34→v35 differs from fresh schema")
	}
	requireIntact(t, s.db)
}
