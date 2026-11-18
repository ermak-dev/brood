package store

// FEATURE: Durable child execution queue and honest joins (#289).
// SPEC: Role-addressed requests pin identity, exact retries do not requeue,
// one worker runs one child FIFO, sending survives restart as uncertain,
// only queued or proven-not-sent children cancel, and only server-computed
// aggregation can close a parent with no outstanding obligations.
// IMPLEMENTATION: Transactional role routing, delivery evidence, FIFO and join gates.
// RESULT: IMPLEMENTED — SQLite, full Go suite and focused race checks pass.
// STATUS: DONE

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func engineChild(t *testing.T, s *Store, p SwarmChildParent, key string) SwarmChildTask {
	t.Helper()
	c, err := s.EnqueueSwarmChild(context.Background(), p, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, key, "test", "Task "+key, childAllow)
	mustNil(t, err)
	return c
}

func engineFixture(t *testing.T) (*Store, SwarmChildParent, SwarmChildPrincipal) {
	t.Helper()
	s, p, e := childFixture(t, "all", 2, 0)
	// Bind before the pass in production; mutate via fixture SQL to retain the pass.
	_, err := s.db.Exec(`INSERT INTO swarm_role_bindings(swarm_id,role,actor_uid,actor_incarnation) VALUES('one','test','b',1)`)
	mustNil(t, err)
	return s, p, e
}

func TestChildEngineQueueFIFOAndRetry(t *testing.T) {
	ctx := context.Background()
	s, p, e := engineFixture(t)
	a := engineChild(t, s, p, "a")
	b := engineChild(t, s, p, "b")
	if a.State != "pending" || a.Delivery != "queued" || a.Role != "test" || a.Executor != e {
		t.Fatalf("queued: %+v", a)
	}
	if _, err := s.StartSwarmChild(ctx, p, b.RequestID, e, b.Revision, childAllow); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("overtook FIFO: %v", err)
	}
	const n = 8
	results := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.StartSwarmChild(ctx, p, a.RequestID, e, a.Revision, childAllow)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrSwarmChildConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d claims", winners)
	}
	repeat := engineChild(t, s, p, "a")
	if repeat.ID != a.ID || repeat.Delivery != "sending" {
		t.Fatalf("retry requeued: %+v", repeat)
	}
	if _, err := s.EnqueueSwarmChild(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "a", "test", "different", childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatal(err)
	}
	mustNil(t, s.RecordSwarmChildDelivery(ctx, p, "a", "delivered", "receipt confirmed"))
	j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	_, err = s.CompleteSwarmChild(ctx, p, "a", e, j.Children[0].Revision, "success", "first report", childAllow)
	mustNil(t, err)
	_, err = s.StartSwarmChild(ctx, p, "b", e, b.Revision, childAllow)
	mustNil(t, err)
}

func TestChildEngineNotSentVersusUnknown(t *testing.T) {
	ctx := context.Background()
	for _, delivery := range []string{"not_sent", "unconfirmed", "sending", "delivered"} {
		t.Run(delivery, func(t *testing.T) {
			s, p, e := engineFixture(t)
			c := engineChild(t, s, p, "a")
			c, err := s.StartSwarmChild(ctx, p, "a", e, c.Revision, childAllow)
			mustNil(t, err)
			if delivery != "sending" {
				mustNil(t, s.RecordSwarmChildDelivery(ctx, p, "a", delivery, "public reason"))
			}
			mustNil(t, s.RecoverSwarmChildDeliveries(ctx))
			j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
			mustNil(t, err)
			c = j.Children[0]
			if delivery == "sending" && c.Delivery != "unconfirmed" {
				t.Fatalf("restart did not retain uncertainty: %+v", c)
			}
			_, err = s.CancelSwarmChild(ctx, p, "a", c.Revision, childAllow)
			if delivery == "not_sent" {
				mustNil(t, err)
			} else if !errors.Is(err, ErrSwarmChildConflict) {
				t.Fatalf("cancel erased possible effect: %v", err)
			}
			if _, err := s.JoinSwarmChildren(ctx, p, childAllow); err != nil {
				t.Fatal(err)
			}
			if v := stepOf(t, s, 1); v.State == "completed" {
				t.Fatal("incomplete join closed parent")
			}
		})
	}
}

func TestChildEngineServerComputedParentResult(t *testing.T) {
	ctx := context.Background()
	s, p, e := engineFixture(t)
	for _, tc := range []struct{ key, outcome string }{{"a", "success"}, {"b", "fail"}} {
		c := engineChild(t, s, p, tc.key)
		c, err := s.StartSwarmChild(ctx, p, tc.key, e, c.Revision, childAllow)
		mustNil(t, err)
		_, err = s.CompleteSwarmChild(ctx, p, tc.key, e, c.Revision, tc.outcome, strings.Repeat(tc.key, MaxStepReportBytes), childAllow)
		mustNil(t, err)
	}
	if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "parent", "a", 1, "success", "forged success"); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("parent forged aggregate: %v", err)
	}
	j, err := s.JoinSwarmChildren(ctx, p, childAllow)
	mustNil(t, err)
	v := stepOf(t, s, 1)
	if j.Evaluation.Open != 0 || v.Outcome == nil || *v.Outcome != "fail" || v.State != "completed" || len(v.Report) > MaxStepReportBytes || !strings.Contains(v.Report, "truncated") || !strings.Contains(v.Report, "Child ") {
		t.Fatalf("join %+v parent %+v", j, v)
	}
}

func TestChildEngineCompletionProvesDelivery(t *testing.T) {
	ctx := context.Background()
	s, p, e := engineFixture(t)
	c := engineChild(t, s, p, "a")
	c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
	mustNil(t, err)
	c, err = s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "Report received before receipt", childAllow)
	mustNil(t, err)
	if c.Delivery != "delivered" {
		t.Fatalf("executor report did not resolve delivery: %+v", c)
	}
	mustNil(t, s.RecordSwarmChildDelivery(ctx, p, c.RequestID, "unconfirmed", "late transport error"))
	j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if j.Children[0].Delivery != "delivered" {
		t.Fatal("late sender downgraded completion proof")
	}
}

func TestChildEngineRequiresJoinAuthority(t *testing.T) {
	ctx := context.Background()
	s, p, e := engineFixture(t)
	for _, key := range []string{"a", "b"} {
		c := engineChild(t, s, p, key)
		c, err := s.StartSwarmChild(ctx, p, key, e, c.Revision, childAllow)
		mustNil(t, err)
		_, err = s.CompleteSwarmChild(ctx, p, key, e, c.Revision, "success", "report", childAllow)
		mustNil(t, err)
	}
	j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "parent", "a", 1, "success", ChildJoinReport(j)); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("generic completion bypassed join authorization: %v", err)
	}
}

func TestChildEngineRoleDriftAndUnsentCancel(t *testing.T) {
	ctx := context.Background()
	s, p, e := engineFixture(t)
	c := engineChild(t, s, p, "a")
	_, err := s.db.Exec(`UPDATE swarm_role_bindings SET actor_uid='a' WHERE role='test'`)
	mustNil(t, err)
	if _, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow); err == nil {
		t.Fatal("role drift accepted")
	}
	_, err = s.CancelSwarmChild(ctx, p, c.RequestID, c.Revision, childAllow)
	mustNil(t, err)
}
