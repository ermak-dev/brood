package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func passContextQuestion(t *testing.T, s *Store) SwarmRequest {
	t.Helper()
	q, err := s.OpenActorRequest(context.Background(), ActorRequestInput{SwarmID: "one", ActorUID: "a", ActorIncarnation: 1, Kind: "text", Question: "Which invariant should I preserve?"})
	mustNil(t, err)
	if q.Sequence != nil || q.Origin != "actor" {
		t.Fatal("pass context turned a global actor question into a graph question")
	}
	return q
}

func TestActorRequestRetainsExactPassContextWithoutOwningTheStep(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	mustStart(t, s, "release")
	first := passContextQuestion(t, s)
	got, err := s.GetSwarmRequestPassContext(ctx, "one", first.ID)
	mustNil(t, err)
	if !reflect.DeepEqual(got, SwarmRequestPassContext{SwarmID: "one", RequestID: first.ID, Pass: 1, ObservedSequence: 1}) {
		t.Fatalf("question lost exact current pass context: %+v", got)
	}
	answerNode(t, s, 1, "success")
	second := passContextQuestion(t, s)
	got, err = s.GetSwarmRequestPassContext(ctx, "one", second.ID)
	mustNil(t, err)
	if got.Pass != 1 || got.ObservedSequence != 2 || stepCount(t, s) != 2 {
		t.Fatalf("context used first step or created a visit: %+v", got)
	}
	stopOne(t, s)
	mustStart(t, s, "release")
	third := passContextQuestion(t, s)
	got, err = s.GetSwarmRequestPassContext(ctx, "one", third.ID)
	mustNil(t, err)
	if got.Pass != 3 || got.ObservedSequence != 3 {
		t.Fatalf("new pass inherited previous question context: %+v", got)
	}
	old, err := s.GetSwarmRequestPassContext(ctx, "one", first.ID)
	mustNil(t, err)
	if old.Pass != 1 || old.ObservedSequence != 1 {
		t.Fatal("restart reassigned a historical question to today's pass")
	}
	if _, err := s.GetSwarmRequestPassContext(ctx, "two", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("pass context leaked a foreign swarm's question")
	}
}

func TestActorRequestOutsideActivePassRemainsUnscoped(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	before := passContextQuestion(t, s)
	mustStart(t, s, "release")
	stopOne(t, s)
	after := passContextQuestion(t, s)
	for _, q := range []SwarmRequest{before, after} {
		if _, err := s.GetSwarmRequestPassContext(ctx, "one", q.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("idle/stopped question was assigned a fabricated pass: %v", err)
		}
		retained, err := s.GetSwarmRequest(ctx, "one", q.ID)
		mustNil(t, err)
		if retained.Question != q.Question || retained.Sequence != nil || retained.State != "open" {
			t.Fatal("unscoped context changed the ordinary question contract")
		}
	}
}

func TestActorRequestPassContextRejectsForgedSourcesAndSurvivesSourceRemoval(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	legacy := passContextQuestion(t, s)
	mustStart(t, s, "release")
	node := openNodeRequestOf(t, s, 1)
	for _, attempt := range []struct {
		id, swarm      string
		pass, sequence int64
	}{
		{legacy.ID, "two", 1, 1},
		{legacy.ID, "one", 2, 2},
		{legacy.ID, "one", 1, 2},
		{legacy.ID, "one", 0, 1},
		{node.ID, "one", 1, 1},
		{"missing", "one", 1, 1},
	} {
		if _, err := s.db.Exec(`INSERT INTO swarm_request_pass_contexts VALUES(?,?,?,?)`, attempt.id, attempt.swarm, attempt.pass, attempt.sequence); err == nil {
			t.Fatalf("invalid context source accepted: %+v", attempt)
		}
	}
	q := passContextQuestion(t, s)
	before, err := s.GetSwarmRequestPassContext(ctx, "one", q.ID)
	mustNil(t, err)
	_, err = s.db.Exec(`DELETE FROM managed_actors WHERE uid='a'`)
	mustNil(t, err)
	_, err = s.db.Exec(`DELETE FROM swarms WHERE id='one'`)
	mustNil(t, err)
	after, err := s.GetSwarmRequestPassContext(ctx, "one", q.ID)
	mustNil(t, err)
	if after != before {
		t.Fatal("mutable source deletion erased or rewrote retained pass context")
	}
}

func TestActorRequestPassContextFailureRollsQuestionAndNotificationBack(t *testing.T) {
	ctx := context.Background()
	s := goalStore(t)
	mustStart(t, s, "release")
	_, err := s.db.Exec(`CREATE TRIGGER reject_request_pass_context BEFORE INSERT ON swarm_request_pass_contexts BEGIN SELECT RAISE(ABORT,'context unavailable'); END`)
	mustNil(t, err)
	before := dumpTables(t, s, "swarm_requests", "swarm_request_pass_contexts", "telegram_outbox")
	_, err = s.OpenActorRequest(ctx, ActorRequestInput{SwarmID: "one", ActorUID: "a", ActorIncarnation: 1, Kind: "text", Question: "Do not leave a question without its captured context."})
	if err == nil {
		t.Fatal("question committed after required context capture failed")
	}
	if dumpTables(t, s, "swarm_requests", "swarm_request_pass_contexts", "telegram_outbox") != before {
		t.Fatal("context failure left partial question or notification state")
	}
}

func TestActorRequestPassContextIsImmutableAndDoesNotReferenceMutableInventory(t *testing.T) {
	s := goalStore(t)
	mustStart(t, s, "release")
	q := passContextQuestion(t, s)
	for _, statement := range []string{
		`UPDATE swarm_request_pass_contexts SET pass_sequence=99`,
		`DELETE FROM swarm_request_pass_contexts`,
		`INSERT OR REPLACE INTO swarm_request_pass_contexts SELECT * FROM swarm_request_pass_contexts`,
	} {
		if _, err := s.db.Exec(statement); err == nil {
			t.Fatalf("immutable request pass context accepted %q", statement)
		}
	}
	got, err := s.GetSwarmRequestPassContext(context.Background(), "one", q.ID)
	mustNil(t, err)
	if got.Pass != 1 || got.ObservedSequence != 1 {
		t.Fatal("failed mutation changed retained pass context")
	}
	rows, err := s.db.Query(`PRAGMA foreign_key_list(swarm_request_pass_contexts)`)
	mustNil(t, err)
	defer rows.Close()
	if rows.Next() {
		t.Fatal("immutable context would block deletion through a mutable source foreign key")
	}
	mustNil(t, rows.Err())
}
