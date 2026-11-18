package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func childAllow(context.Context, SwarmChildAuthorization) error { return nil }

func childFixture(t *testing.T, mode string, total, quorum int) (*Store, SwarmChildParent, SwarmChildPrincipal) {
	t.Helper()
	s := dispatchStore(t)
	attestOne(t, s, "parent")
	// Как диспетчер, снимаем подтверждённую workspace-lease до работы актора.
	op, err := s.CurrentActorOperation(context.Background(), "a")
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(context.Background(), op, true))
	p := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: "one", SwarmID: "one", AtespaceUID: "native-one", ActorUID: "a", Incarnation: 1}, Pass: 1, Sequence: 1, RequestID: "parent"}
	executor := p.Principal
	executor.ActorUID = "b"
	_, err = s.CreateSwarmChildJoin(context.Background(), p, SwarmChildJoinPolicy{Mode: mode, Total: total, Quorum: quorum, Remaining: "retain"}, childAllow)
	mustNil(t, err)
	return s, p, executor
}

func reserveChild(t *testing.T, s *Store, p SwarmChildParent, e SwarmChildPrincipal, key string) SwarmChildTask {
	t.Helper()
	c, err := s.ReserveSwarmChild(context.Background(), p, key, "Implement the parser test.", e, childAllow)
	mustNil(t, err)
	return c
}

func finishChild(t *testing.T, s *Store, p SwarmChildParent, e SwarmChildPrincipal, key, outcome string) SwarmChildTask {
	t.Helper()
	ctx := context.Background()
	c := reserveChild(t, s, p, e, key)
	c, err := s.StartSwarmChild(ctx, p, key, e, c.Revision, childAllow)
	mustNil(t, err)
	c, err = s.CompleteSwarmChild(ctx, p, key, e, c.Revision, outcome, "Child result\nverified.", childAllow)
	mustNil(t, err)
	return c
}

func TestSwarmChildJoinEvaluation(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		total, quorum int
		states        []string
		want          string
		open          int
	}{
		{"all", 1, 0, []string{"success"}, "ready_success", 0},
		{"all", 2, 0, []string{"success", "running"}, "pending", 1},
		{"all", 2, 0, []string{"fail", "running"}, "ready_fail", 1},
		{"all", 2, 0, []string{"success", "cancelled"}, "ready_fail", 0},
		{"any", 2, 0, []string{"success", "pending"}, "ready_success", 1},
		{"any", 2, 0, []string{"fail", "running"}, "pending", 1},
		{"any", 2, 0, []string{"fail", "cancelled"}, "ready_fail", 0},
		{"quorum", 3, 2, []string{"success", "success", "running"}, "ready_success", 1},
		{"quorum", 3, 2, []string{"fail", "cancelled", "running"}, "ready_fail", 1},
		{"quorum", 3, 2, []string{"success", "fail", "running"}, "pending", 1},
		{"quorum", 2, 1, []string{"success", "pending"}, "ready_success", 1},
		{"quorum", 2, 2, []string{"success", "fail"}, "ready_fail", 0},
		{"any", 2, 0, []string{"success"}, "pending", 1},
		{"all", 1, 0, nil, "pending", 1},
	} {
		t.Run(fmt.Sprintf("%s-%v", tc.mode, tc.states), func(t *testing.T) {
			var children []SwarmChildTask
			for _, state := range tc.states {
				c := SwarmChildTask{State: state}
				if state == "success" || state == "fail" {
					c.State = "completed"
					c.Outcome = state
				}
				children = append(children, c)
			}
			got, err := EvaluateSwarmChildJoin(SwarmChildJoinPolicy{Mode: tc.mode, Total: tc.total, Quorum: tc.quorum, Remaining: "retain"}, children)
			if err != nil || got.State != tc.want || got.Open != tc.open {
				t.Fatalf("got %+v, %v; want %s open=%d", got, err, tc.want, tc.open)
			}
		})
	}
	for _, p := range []SwarmChildJoinPolicy{
		{}, {Mode: "all", Total: 0, Remaining: "retain"}, {Mode: "all", Total: 65, Remaining: "retain"},
		{Mode: "quorum", Total: 2, Quorum: 0, Remaining: "retain"}, {Mode: "quorum", Total: 2, Quorum: 3, Remaining: "retain"},
		{Mode: "any", Total: 2, Quorum: 1, Remaining: "retain"}, {Mode: "some", Total: 1, Remaining: "retain"},
		{Mode: "all", Total: 1, Remaining: "cancel"},
	} {
		if _, err := EvaluateSwarmChildJoin(p, nil); err == nil {
			t.Errorf("invalid policy accepted: %+v", p)
		}
	}
	p := SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}
	for _, children := range [][]SwarmChildTask{
		{{State: "completed"}}, {{State: "completed", Outcome: "unknown"}}, {{State: "lost"}}, {{State: "pending", Outcome: "success"}}, {{State: "cancelled", Report: "result"}}, {{State: "pending"}, {State: "pending"}},
	} {
		if _, err := EvaluateSwarmChildJoin(p, children); err == nil {
			t.Errorf("invalid children accepted: %+v", children)
		}
	}
	boundary := make([]SwarmChildTask, 64)
	for i := range boundary {
		boundary[i] = SwarmChildTask{State: "completed", Outcome: "success"}
	}
	result, err := EvaluateSwarmChildJoin(SwarmChildJoinPolicy{Mode: "quorum", Total: 64, Quorum: 64, Remaining: "retain"}, boundary)
	if err != nil || result.State != "ready_success" {
		t.Fatalf("maximum boundary: %+v %v", result, err)
	}
}

func TestSwarmChildConcurrentIdempotencyAndPayload(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	const n = 12
	results := make(chan SwarmChildTask, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.ReserveSwarmChild(ctx, p, "same", "Immutable task", e, childAllow)
			results <- c
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		mustNil(t, err)
	}
	var first SwarmChildTask
	for c := range results {
		if first.ID == "" {
			first = c
		}
		if c.ID != first.ID || c.Revision != 1 || c.State != "pending" {
			t.Fatalf("concurrent retry: %+v / %+v", first, c)
		}
	}
	before := dumpTables(t, s, "swarm_child_tasks", "swarm_child_joins")
	if _, err := s.ReserveSwarmChild(ctx, p, "same", "Changed task", e, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("changed payload: %v", err)
	}
	other := e
	other.ActorUID = "a"
	if _, err := s.ReserveSwarmChild(ctx, p, "same", "Immutable task", other, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("changed executor: %v", err)
	}
	if _, err := s.ReserveSwarmChild(ctx, p, "extra", "Immutable task", e, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("total exceeded: %v", err)
	}
	if after := dumpTables(t, s, "swarm_child_tasks", "swarm_child_joins"); after != before {
		t.Fatal("refused retries changed storage")
	}
	join, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if len(join.Children) != 1 || join.Children[0].Payload != "Immutable task" || join.Parent != p {
		t.Fatalf("join snapshot: %+v", join)
	}
	if _, err := s.CreateSwarmChildJoin(ctx, p, join.Policy, childAllow); err != nil {
		t.Fatal(err)
	}
	policy := join.Policy
	policy.Mode = "any"
	if _, err := s.CreateSwarmChildJoin(ctx, p, policy, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("changed policy: %v", err)
	}
}

func TestSwarmChildCASCompletionAndRetainedEarlyJoin(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"any", "quorum"} {
		t.Run(mode, func(t *testing.T) {
			q := 0
			if mode == "quorum" {
				q = 1
			}
			s, p, e := childFixture(t, mode, 2, q)
			finishChild(t, s, p, e, "first", "success")
			c := reserveChild(t, s, p, e, "second")
			join, err := s.GetSwarmChildJoin(ctx, p, childAllow)
			mustNil(t, err)
			if join.Evaluation.State != "ready_success" || join.Evaluation.Open != 1 || join.Children[1].State != "pending" {
				t.Fatalf("early join lost retained child: %+v", join)
			}
			if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "parent", "a", 1, "success", ""); err == nil {
				t.Fatal("early ready join released parent")
			}
			c, err = s.StartSwarmChild(ctx, p, "second", e, c.Revision, childAllow)
			mustNil(t, err)
			if _, err := s.CancelSwarmChild(ctx, p, "second", c.Revision, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
				t.Fatalf("cancelled active child effect: %v", err)
			}
			if _, err := s.CompleteSwarmChild(ctx, p, "second", e, c.Revision-1, "fail", "failure", childAllow); !errors.Is(err, ErrSwarmChildConflict) {
				t.Fatalf("stale CAS: %v", err)
			}
			done, err := s.CompleteSwarmChild(ctx, p, "second", e, c.Revision, "fail", "failure", childAllow)
			mustNil(t, err)
			retry, err := s.CompleteSwarmChild(ctx, p, "second", e, c.Revision, "fail", "failure", childAllow)
			mustNil(t, err)
			if !reflect.DeepEqual(done, retry) || done.State != "completed" || done.Outcome != "fail" {
				t.Fatalf("completion replay: %+v %+v", done, retry)
			}
			if _, err := s.CompleteSwarmChild(ctx, p, "second", e, c.Revision, "success", "failure", childAllow); !errors.Is(err, ErrSwarmChildConflict) {
				t.Fatalf("changed completion: %v", err)
			}
			mustNil(t, s.CompletePipelineActorVisit(ctx, "one", "one", 1, "parent", "a", 1, "success", "Parent result"))
		})
	}
}

func TestSwarmChildFailureCancellationAndAllJoin(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 2, 0)
	finishChild(t, s, p, e, "failed", "fail")
	c := reserveChild(t, s, p, e, "unstarted")
	c, err := s.CancelSwarmChild(ctx, p, c.RequestID, c.Revision, childAllow)
	mustNil(t, err)
	if c.State != "cancelled" || c.Outcome != "" || c.FinishedAt == "" {
		t.Fatalf("cancellation: %+v", c)
	}
	join, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if join.Evaluation.State != "ready_fail" || join.Evaluation.Open != 0 {
		t.Fatalf("failed all: %+v", join)
	}
	if _, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("cancelled child restarted: %v", err)
	}
}

func TestSwarmChildAuthorizationAndScope(t *testing.T) {
	ctx := context.Background()
	for _, drift := range []string{"nil policy", "denied policy", "parent reset", "executor reset", "parent role rebound", "parent atespace changed", "foreign executor", "wrong pass", "wrong request", "wrong parent", "callback revocation"} {
		t.Run(drift, func(t *testing.T) {
			s, p, e := childFixture(t, "all", 1, 0)
			c := reserveChild(t, s, p, e, "c")
			auth := SwarmChildAuthorize(childAllow)
			switch drift {
			case "nil policy":
				auth = nil
			case "denied policy":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "parent reset":
				setActor(t, s, "a", "incarnation=2")
			case "executor reset":
				setActor(t, s, "b", "incarnation=2")
			case "parent role rebound":
				mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "b"}))
			case "parent atespace changed":
				_, err := s.db.Exec(`UPDATE swarms SET atespace_uid='replacement' WHERE id='one'`)
				mustNil(t, err)
			case "foreign executor":
				e.SwarmID = "two"
			case "wrong pass":
				p.Pass = 2
			case "wrong request":
				p.RequestID = "other"
			case "wrong parent":
				p.Sequence = 2
			case "callback revocation":
				auth = func(txctx context.Context, _ SwarmChildAuthorization) error {
					_, err := s.conn(txctx).ExecContext(txctx, `UPDATE managed_actors SET incarnation=2 WHERE uid='b'`)
					return err
				}
			}
			before := dumpTables(t, s, "swarm_child_tasks", "swarm_child_joins")
			if _, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, auth); err == nil {
				t.Fatal("stale or unauthorized child started")
			}
			if after := dumpTables(t, s, "swarm_child_tasks", "swarm_child_joins"); after != before {
				t.Fatal("refusal mutated children")
			}
		})
	}
	s, p, e := childFixture(t, "all", 1, 0)
	seen := false
	_, err := s.ReserveSwarmChild(ctx, p, "authorized", "specific payload", e, func(txctx context.Context, a SwarmChildAuthorization) error {
		if _, ok := txctx.Value(txKey{}).(*sql.Tx); !ok {
			t.Fatal("authorization outside transaction")
		}
		if a.Action != "reserve" || a.Caller != p.Principal || a.Parent != p || a.Executor != e || a.RequestID != "authorized" || a.Payload != "specific payload" {
			t.Fatalf("policy context: %+v", a)
		}
		seen = true
		return nil
	})
	mustNil(t, err)
	if !seen {
		t.Fatal("policy skipped")
	}
}

func TestSwarmChildReportValidation(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	c := reserveChild(t, s, p, e, "c")
	c, err := s.StartSwarmChild(ctx, p, "c", e, c.Revision, childAllow)
	mustNil(t, err)
	for _, report := range []string{strings.Repeat("x", MaxStepReportBytes+1), "bad\x00report", string([]byte{255})} {
		if _, err := s.CompleteSwarmChild(ctx, p, "c", e, c.Revision, "success", report, childAllow); !errors.Is(err, ErrStepReportInvalid) {
			t.Fatalf("invalid report: %v", err)
		}
	}
	c, err = s.CompleteSwarmChild(ctx, p, "c", e, c.Revision, "success", strings.Repeat("x", MaxStepReportBytes), childAllow)
	mustNil(t, err)
	if len(c.Report) != MaxStepReportBytes {
		t.Fatal("report boundary was truncated")
	}
}

func TestSwarmChildRollbackRestartAndLatePass(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	rollback := errors.New("rollback")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		reserve, err := s.ReserveSwarmChild(ctx, p, "rolled-back", "task", e, childAllow)
		if err != nil {
			return err
		}
		if reserve.ID == "" {
			t.Fatal("missing ID")
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	join, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if len(join.Children) != 0 {
		t.Fatal("rolled-back child survived")
	}
	c := reserveChild(t, s, p, e, "same")
	c, err = s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
	mustNil(t, err)
	var file string
	var seq int
	var dbname string
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbname, &file))
	mustNil(t, s.Close())
	reopened, err := Open(ctx, file)
	mustNil(t, err)
	defer reopened.Close()
	join, err = reopened.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if len(join.Children) != 1 || !reflect.DeepEqual(c, join.Children[0]) {
		t.Fatalf("restart changed child: %+v", join)
	}
	done, err := reopened.CompleteSwarmChild(ctx, p, "same", e, c.Revision, "success", "result", childAllow)
	mustNil(t, err)
	mustNil(t, reopened.CompletePipelineActorVisit(ctx, "one", "one", 1, "parent", "a", 1, "success", ""))
	_, err = startOne(reopened, "build-first")
	mustNil(t, err)
	before := dumpTables(t, reopened, "swarm_steps", "swarm_child_tasks")
	if _, err := reopened.CompleteSwarmChild(ctx, p, "same", e, done.Revision-1, "success", "result", childAllow); err == nil {
		t.Fatal("late completion accepted in another pass")
	}
	if after := dumpTables(t, reopened, "swarm_steps", "swarm_child_tasks"); after != before {
		t.Fatal("late completion changed another pass")
	}
	a, err := reopened.ReserveActorStep(ctx, "one", "parent-next", "swarm:one")
	mustNil(t, err)
	mustNil(t, reopened.RecordActorStepAttestation(ctx, a, startOrigin, startCommit))
	p.Pass = a.Sequence
	p.Sequence = a.Sequence
	p.RequestID = a.RequestID
	_, err = reopened.CreateSwarmChildJoin(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}, childAllow)
	mustNil(t, err)
	next := reserveChild(t, reopened, p, e, "same")
	if next.ID == c.ID {
		t.Fatal("request ID reused across parents")
	}
}
