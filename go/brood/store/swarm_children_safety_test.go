package store

// FEATURE: Границы конкурентного исполнения и отмены дочерних задач (#289).
// SPEC: Единственный CAS начинает работу; повтор claim не даёт второй отправки.
// Отмена pending разрешается после reset исполнителя, но policy видит точную
// исходную личность. Late completion, отзыв прав и рестарт не теряют ограды.
// IMPLEMENTATION: Одноразовый start CAS, точный policy context и SQL fences.
// RESULT: IMPLEMENTED — прежние повторные claims и пустая личность отвергаются.
// STATUS: DONE

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestSwarmChildStartHasOneClaim(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	c := reserveChild(t, s, p, e, "one-claim")
	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	claims := 0
	for err := range errs {
		if err == nil {
			claims++
		} else if !errors.Is(err, ErrSwarmChildConflict) {
			t.Fatal(err)
		}
	}
	if claims != 1 {
		t.Fatalf("start gave %d execution claims; want exactly one", claims)
	}
}

func TestSwarmChildCancelPolicySeesStaleExecutor(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	c := reserveChild(t, s, p, e, "cancel")
	setActor(t, s, "b", "incarnation=2")
	checked := false
	_, err := s.CancelSwarmChild(ctx, p, c.RequestID, c.Revision, func(ctx context.Context, a SwarmChildAuthorization) error {
		if a.Action != "cancel" || a.Caller != p.Principal || a.Executor != e || a.RequestID != c.RequestID || a.Payload != c.Payload {
			t.Fatalf("incomplete cancellation policy: %+v", a)
		}
		checked = true
		return nil
	})
	mustNil(t, err)
	if !checked {
		t.Fatal("cancellation skipped policy")
	}
}

func TestSwarmChildMissingExecutorIsDenied(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	e.ActorUID = ""
	_, err := s.ReserveSwarmChild(ctx, p, "invalid", "task", e, childAllow)
	if !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatalf("empty executor: %v", err)
	}
}

func TestSwarmChildLateCompletionRefusesIdentityDrift(t *testing.T) {
	ctx := context.Background()
	for _, drift := range []string{"executor reset", "parent reset", "role rebound", "wrong swarm", "wrong parent request", "wrong child caller", "denied callback", "nil callback"} {
		t.Run(drift, func(t *testing.T) {
			s, p, e := childFixture(t, "all", 1, 0)
			c := reserveChild(t, s, p, e, "running")
			c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
			mustNil(t, err)
			auth := SwarmChildAuthorize(childAllow)
			switch drift {
			case "executor reset":
				setActor(t, s, "b", "incarnation=2")
			case "parent reset":
				setActor(t, s, "a", "incarnation=2")
			case "role rebound":
				mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "b"}))
			case "wrong swarm":
				p.Principal.SwarmID = "two"
				p.Principal.ProjectID = "two"
				p.Principal.AtespaceUID = "native-two"
			case "wrong parent request":
				p.RequestID = "different"
			case "wrong child caller":
				e = p.Principal
			case "denied callback":
				auth = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "nil callback":
				auth = nil
			}
			before := dumpTables(t, s, "swarm_child_tasks", "swarm_steps")
			if _, err := s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "late", auth); err == nil {
				t.Fatal("stale completion accepted")
			}
			if after := dumpTables(t, s, "swarm_child_tasks", "swarm_steps"); after != before {
				t.Fatal("stale completion wrote state")
			}
		})
	}
}

func TestSwarmChildJoinAndTaskRefusalsAreAtomic(t *testing.T) {
	ctx := context.Background()
	s := dispatchStore(t)
	attestOne(t, s, "parent")
	p := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: "one", SwarmID: "one", AtespaceUID: "native-one", ActorUID: "a", Incarnation: 1}, Pass: 1, Sequence: 1, RequestID: "parent"}
	policy := SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}
	for _, auth := range []SwarmChildAuthorize{nil, func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }} {
		if _, err := s.CreateSwarmChildJoin(ctx, p, policy, auth); !errors.Is(err, ErrSwarmChildDenied) {
			t.Fatalf("create authority: %v", err)
		}
	}
	rollback := errors.New("roll back join")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.CreateSwarmChildJoin(ctx, p, policy, childAllow)
		if err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var count int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM swarm_child_joins`).Scan(&count))
	if count != 0 {
		t.Fatal("rolled-back join survived")
	}
	_, err = s.CreateSwarmChildJoin(ctx, p, policy, childAllow)
	mustNil(t, err)
	e := p.Principal
	e.ActorUID = "b"
	for _, tc := range []struct{ key, payload string }{
		{"", "task"}, {strings.Repeat("r", 129), "task"}, {"bad key", "task"}, {"key", ""}, {"key", "  \n"}, {"key", strings.Repeat("x", 4097)}, {"key", "task\x00"}, {"key", string([]byte{255})},
	} {
		if _, err := s.ReserveSwarmChild(ctx, p, tc.key, tc.payload, e, childAllow); err == nil {
			t.Errorf("invalid child accepted: %q payload bytes=%d", tc.key, len(tc.payload))
		}
	}
	if _, err := s.ReserveSwarmChild(ctx, p, "valid", "task", e, nil); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatalf("reserve missing authority: %v", err)
	}
	if _, err := s.GetSwarmChildJoin(ctx, p, nil); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatalf("read missing authority: %v", err)
	}
	c, err := s.ReserveSwarmChild(ctx, p, strings.Repeat("r", 128), strings.Repeat("x", 4096), e, childAllow)
	mustNil(t, err)
	if len(c.Payload) != 4096 || len(c.RequestID) != 128 {
		t.Fatal("boundary payload truncated")
	}
}

func TestSwarmChildrenRawSQLFences(t *testing.T) {
	for _, recursive := range []int{0, 1} {
		t.Run(fmt.Sprint(recursive), func(t *testing.T) {
			s, p, e := childFixture(t, "all", 1, 0)
			ctx := context.Background()
			c := reserveChild(t, s, p, e, "c")
			_, err := s.db.Exec(fmt.Sprintf(`PRAGMA recursive_triggers=%d`, recursive))
			mustNil(t, err)
			for _, statement := range []string{
				`DELETE FROM swarm_child_tasks`,
				`UPDATE swarm_child_tasks SET payload='replacement',revision=revision+1`,
				`UPDATE swarm_child_tasks SET actor_incarnation=2,revision=revision+1`,
				`UPDATE swarm_child_tasks SET state='running'`,
				`UPDATE swarm_child_tasks SET state='completed',outcome='success',finished_at='now',revision=revision+1`,
				`UPDATE swarm_child_tasks SET state='running',report='premature',revision=revision+1`,
				`INSERT OR REPLACE INTO swarm_child_tasks SELECT * FROM swarm_child_tasks`,
				`INSERT OR REPLACE INTO swarm_child_joins SELECT * FROM swarm_child_joins`,
				`INSERT OR REPLACE INTO swarm_steps SELECT * FROM swarm_steps WHERE swarm_id='one'`,
				`UPDATE swarms SET state='deleted' WHERE id='one'`,
				`DELETE FROM swarms WHERE id='one'`,
			} {
				if _, err := s.db.Exec(statement); err == nil {
					t.Errorf("fence accepted %s", statement)
				}
			}
			c, err = s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
			mustNil(t, err)
			if _, err := s.db.Exec(`UPDATE swarm_child_tasks SET state='cancelled',finished_at='now',revision=revision+1`); err == nil {
				t.Fatal("raw running cancellation accepted")
			}
			c, err = s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "fail", "failure", childAllow)
			mustNil(t, err)
			before := dumpTables(t, s, "swarm_child_tasks")
			for _, statement := range []string{
				`UPDATE swarm_child_tasks SET state='pending',outcome=NULL,report=NULL,finished_at=NULL,revision=revision+1`,
				`UPDATE swarm_child_tasks SET report='replacement',revision=revision+1`,
				`UPDATE swarm_child_tasks SET outcome='success',revision=revision+1`,
			} {
				if _, err := s.db.Exec(statement); err == nil {
					t.Errorf("terminal fence accepted %s", statement)
				}
			}
			if after := dumpTables(t, s, "swarm_child_tasks"); after != before {
				t.Fatal("terminal writes changed task")
			}
		})
	}
}

func TestSwarmChildCompletionRollbackAndConcurrentPayloadConflict(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	const n = 8
	type result struct {
		task SwarmChildTask
		err  error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := s.ReserveSwarmChild(ctx, p, "same", fmt.Sprintf("task %d", i), e, childAllow)
			results <- result{c, err}
		}(i)
	}
	wg.Wait()
	close(results)
	var c SwarmChildTask
	accepted := 0
	for r := range results {
		if r.err == nil {
			c = r.task
			accepted++
		} else if !errors.Is(r.err, ErrSwarmChildConflict) {
			t.Fatal(r.err)
		}
	}
	if accepted != 1 {
		t.Fatalf("different payloads accepted %d", accepted)
	}
	c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
	mustNil(t, err)
	before := dumpTables(t, s, "swarm_child_tasks")
	rollback := errors.New("completion rollback")
	err = s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "result", childAllow)
		if err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if after := dumpTables(t, s, "swarm_child_tasks"); after != before {
		t.Fatal("completion escaped rollback")
	}
	done, err := s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "result", childAllow)
	mustNil(t, err)
	replay, err := s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "result", childAllow)
	mustNil(t, err)
	if !reflect.DeepEqual(done, replay) {
		t.Fatal("completion retry changed record")
	}
}
