package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Целостность запросов роя против прямой записи и устаревших акторов (#209).
// SPEC: ответ и отмена не стираются DELETE/REPLACE при любом recursive_triggers,
// но каскад удаления роя работает; прямая отмена узлового запроса не оставляет
// ждущий шаг; остановленный граф с отменённым шлюзом не возобновляется;
// запрос актора открывается и получает ответ только от актуальной
// инкарнации живого актора; повтор ответа принимает только его автор с
// действующими правами и тем же исходом и текстом.
// IMPLEMENTATION: триггеры swarm_requests и swarms, проверка актора в
// транзакции, повтор в AnswerSwarmRequest.
// RESULT: IMPLEMENTED
// STATUS: DONE

func setRecursive(t *testing.T, s *Store, mode string) {
	t.Helper()
	if _, err := s.db.Exec(`PRAGMA recursive_triggers=` + mode); err != nil {
		t.Fatal(err)
	}
}

func snapshotRequests(t *testing.T, s *Store) []SwarmRequest {
	t.Helper()
	all, err := s.SwarmRequests(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// requestsOfEveryKind: открытый и отвеченный узловой, отвеченный,
// отменённый и открытый запросы актора.
func requestsOfEveryKind(t *testing.T) (*Store, map[string]SwarmRequest) {
	t.Helper()
	ctx := context.Background()
	s, node := requestStore(t)
	got := map[string]SwarmRequest{"node-open": node}
	answered := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, answered, "u", "", "yes"))
	cancelled := actorRequest(t, s, "text", nil)
	mustNil(t, s.CancelSwarmRequest(ctx, "s", cancelled.ID, "u", allow))
	open := actorRequest(t, s, "text", nil)
	got["actor-answered"], got["actor-cancelled"], got["actor-open"] = answered, cancelled, open
	for k, id := range map[string]string{"actor-answered": answered.ID, "actor-cancelled": cancelled.ID, "actor-open": open.ID} {
		r, err := s.GetSwarmRequest(ctx, "s", id)
		mustNil(t, err)
		got[k] = r
	}
	return s, got
}

func TestRequestsSurviveDeleteAndReplaceInBothTriggerModes(t *testing.T) {
	for _, mode := range []string{"OFF", "ON"} {
		t.Run("recursive_"+mode, func(t *testing.T) {
			s, requests := requestsOfEveryKind(t)
			setRecursive(t, s, mode)
			// Завершённый узловой запрос тоже: ответ и граф уже продвинулись.
			node := requests["node-open"]
			mustNil(t, answer(s, node, "u", "success", "done"))
			before := snapshotRequests(t, s)
			for name, r := range requests {
				if name == "node-open" {
					r, _ = s.GetSwarmRequest(context.Background(), "s", node.ID)
				}
				for _, statement := range []string{
					`DELETE FROM swarm_requests WHERE id='` + r.ID + `'`,
					`DELETE FROM swarm_requests WHERE swarm_id='s'`,
					`INSERT OR REPLACE INTO swarm_requests SELECT id,swarm_id,sequence,origin,actor_uid,actor_incarnation,kind,question,context,options,'open',created_at,NULL,NULL,NULL,NULL FROM swarm_requests WHERE id='` + r.ID + `'`,
					`REPLACE INTO swarm_requests SELECT id,swarm_id,sequence,origin,actor_uid,actor_incarnation,kind,question,context,options,state,created_at,answered_at,answered_by,outcome,'rewritten' FROM swarm_requests WHERE id='` + r.ID + `'`,
					`INSERT OR IGNORE INTO swarm_requests SELECT id,swarm_id,sequence,origin,actor_uid,actor_incarnation,kind,question,context,options,'open',created_at,NULL,NULL,NULL,NULL FROM swarm_requests WHERE id='` + r.ID + `'`,
				} {
					if _, err := s.db.Exec(statement); err == nil {
						t.Fatalf("%s: request mutated through %s", name, statement)
					}
				}
			}
			// REPLACE по ключу шага: новая строка с другим id вытеснила бы узловой запрос.
			if _, err := s.db.Exec(`INSERT OR REPLACE INTO swarm_requests(id,swarm_id,sequence,origin,kind,question,options,state,created_at) VALUES ('forged','s',1,'node','choice','q',x'5b5d','open','now')`); err == nil {
				t.Fatal("node request replaced through its step key")
			}
			if after := snapshotRequests(t, s); !reflect.DeepEqual(before, after) {
				t.Fatalf("requests changed:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

func TestParentDeletionStillCascadesRequests(t *testing.T) {
	for _, mode := range []string{"OFF", "ON"} {
		t.Run("recursive_"+mode, func(t *testing.T) {
			s, requests := requestsOfEveryKind(t)
			setRecursive(t, s, mode)
			// Управляемый актор роя блокирует удаление до своей отставки.
			if _, err := s.db.Exec(`UPDATE managed_actors SET phase='deleted',stage='done' WHERE swarm_id='s'`); err != nil {
				t.Fatal(err)
			}
			mustNil(t, answer(s, requests["node-open"], "u", "success", ""))
			if _, err := s.db.Exec(`DELETE FROM swarms WHERE id='s'`); err != nil {
				t.Fatalf("cascade delete refused: %v", err)
			}
			var n int
			mustNil(t, s.db.QueryRow(`SELECT (SELECT count(*) FROM swarm_requests)+(SELECT count(*) FROM swarm_steps)`).Scan(&n))
			if n != 0 {
				t.Fatalf("rows left after cascade: %d", n)
			}
		})
	}
}

func TestDirectCancelCannotLeaveWaitingStep(t *testing.T) {
	for _, mode := range []string{"OFF", "ON"} {
		t.Run("recursive_"+mode, func(t *testing.T) {
			s, node := requestStore(t)
			setRecursive(t, s, mode)
			if _, err := s.db.Exec(`UPDATE swarm_requests SET state='cancelled' WHERE id=?`, node.ID); err == nil {
				t.Fatal("node request cancelled while the graph waits on it")
			}
			if got := openNode(t, s); got.ID != node.ID {
				t.Fatalf("request: %+v", got)
			}
			if !reflect.DeepEqual(stepStates(t, s), []string{"ask:waiting"}) {
				t.Fatalf("steps: %v", stepStates(t, s))
			}
		})
	}
}

func TestStoppingTheGraphCancelsItsGateAtomically(t *testing.T) {
	s, node := requestStore(t)
	// Даже прямая запись состояния исполнения закрывает шлюз в том же операторе.
	if _, err := s.db.Exec(`UPDATE swarms SET execution='stopped' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSwarmRequest(context.Background(), "s", node.ID)
	if err != nil || got.State != "cancelled" {
		t.Fatalf("gate after stop: %+v %v", got, err)
	}
	if n := len(openRequests(t, s)); n != 0 {
		t.Fatalf("open requests after stop: %d", n)
	}
}

func TestStoppedGraphWithCancelledGateDoesNotResume(t *testing.T) {
	ctx := context.Background()
	s, node := requestStore(t)
	mustNil(t, s.SetSwarmExecution(ctx, "s", "stopped"))
	for _, execution := range []string{"running", "waiting"} {
		if err := s.SetSwarmExecution(ctx, "s", execution); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("resume as %s: %v", execution, err)
		}
		if _, err := s.db.Exec(`UPDATE swarms SET execution=? WHERE id='s'`, execution); err == nil {
			t.Fatalf("direct resume as %s accepted", execution)
		}
	}
	sw, err := s.GetSwarm(ctx, "s")
	if err != nil || sw.Execution != "stopped" {
		t.Fatalf("execution: %+v %v", sw, err)
	}
	if got, _ := s.GetSwarmRequest(ctx, "s", node.ID); got.State != "cancelled" {
		t.Fatalf("gate: %+v", got)
	}
	if err := answer(s, node, "u", "success", ""); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("answer: %v", err)
	}
	// Граф без ждущего шлюза возобновляется как прежде.
	s2 := swarmTestStore(t)
	mustNil(t, attachTestGraph(ctx, s2, "s", requestGraph))
	mustNil(t, s2.SetSwarmExecution(ctx, "s", "stopped"))
	mustNil(t, s2.SetSwarmExecution(ctx, "s", "running"))
}

func TestOpenActorRequestChecksCurrentActor(t *testing.T) {
	ctx := context.Background()
	open := func(s *Store, uid string, incarnation int64) error {
		_, err := s.OpenActorRequest(ctx, ActorRequestInput{SwarmID: "s", ActorUID: uid, ActorIncarnation: incarnation, Kind: "text", Question: "q"})
		return err
	}
	count := func(s *Store) int { return len(snapshotRequests(t, s)) }
	s, _ := requestStore(t)
	actorRequest(t, s, "text", nil)
	base := count(s)
	if err := open(s, "worker", 2); !errors.Is(err, ErrSwarmRequestStale) {
		t.Fatalf("future incarnation: %v", err)
	}
	for phase, want := range map[string]bool{"running": true, "provisioning": true, "stopped": false, "stopping": false, "error": false, "deleting": false, "deleted": false} {
		if _, err := s.db.Exec(`UPDATE managed_actors SET phase=? WHERE uid='worker'`, phase); err != nil {
			t.Fatal(err)
		}
		err := open(s, "worker", 1)
		if want {
			mustNil(t, err)
			base++
		} else if !errors.Is(err, ErrSwarmRequestStale) {
			t.Fatalf("phase %s: %v", phase, err)
		}
		if count(s) != base {
			t.Fatalf("phase %s changed requests: %d != %d", phase, count(s), base)
		}
	}
	// Новая инкарнация: вопрос старой отвергается, новой принимается.
	if _, err := s.db.Exec(`UPDATE managed_actors SET phase='running',incarnation=2 WHERE uid='worker'`); err != nil {
		t.Fatal(err)
	}
	if err := open(s, "worker", 1); !errors.Is(err, ErrSwarmRequestStale) {
		t.Fatalf("old incarnation: %v", err)
	}
	mustNil(t, open(s, "worker", 2))
	// Актор другого роя и рой не в состоянии active.
	if err := open(s, "ghost", 1); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("foreign actor: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE swarms SET state='deleting' WHERE id='s'`); err != nil {
		t.Fatal(err)
	}
	if err := open(s, "worker", 2); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("inactive swarm: %v", err)
	}
}

func TestOpenRequestQuotaCountsOnlyTheCurrentIncarnation(t *testing.T) {
	ctx := context.Background()
	s, _ := requestStore(t)
	actorRequest(t, s, "text", nil)
	for i := 1; i < maxOpenActorRequests; i++ {
		actorRequest(t, s, "text", nil)
	}
	in := ActorRequestInput{SwarmID: "s", ActorUID: "worker", ActorIncarnation: 1, Kind: "text", Question: "q"}
	// Предел — отдельная причина (#210): гость получает её публичным текстом.
	if _, err := s.OpenActorRequest(ctx, in); !errors.Is(err, ErrSwarmRequestInvalid) || !errors.Is(err, ErrSwarmRequestLimit) {
		t.Fatalf("over quota: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE managed_actors SET incarnation=2 WHERE uid='worker'`); err != nil {
		t.Fatal(err)
	}
	in.ActorIncarnation = 2
	if _, err := s.OpenActorRequest(ctx, in); err != nil {
		t.Fatalf("new incarnation blocked by stale requests: %v", err)
	}
}

func TestAnswerToStaleActorRequestIsRefusedWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]string{
		"new incarnation": `UPDATE managed_actors SET incarnation=2 WHERE uid='worker'`,
		"deleted":         `UPDATE managed_actors SET phase='deleted',stage='done' WHERE uid='worker'`,
	} {
		t.Run(name, func(t *testing.T) {
			s, node := requestStore(t)
			req := actorRequest(t, s, "text", nil)
			if _, err := s.db.Exec(mutate); err != nil {
				t.Fatal(err)
			}
			before := snapshotRequests(t, s)
			if err := answer(s, req, "u", "", "late"); !errors.Is(err, ErrSwarmRequestStale) {
				t.Fatalf("answer: %v", err)
			}
			if after := snapshotRequests(t, s); !reflect.DeepEqual(before, after) {
				t.Fatalf("stale answer wrote: %+v", after)
			}
			if got := openNode(t, s); got.ID != node.ID {
				t.Fatalf("node request: %+v", got)
			}
			// Оператор закрывает устаревший вопрос отменой.
			mustNil(t, s.CancelSwarmRequest(ctx, "s", req.ID, "u", allow))
		})
	}
}

func TestAnswerRetryNeedsSameAuthorSameContentAndCurrentRights(t *testing.T) {
	ctx := context.Background()
	s, node := requestStore(t)
	mustNil(t, answer(s, node, "ed", "success", "ship it"))
	stored, err := s.GetSwarmRequest(ctx, "s", node.ID)
	mustNil(t, err)
	steps := stepStates(t, s)
	// Точный повтор автором ничего не пишет.
	mustNil(t, answer(s, node, "ed", "success", "ship it"))
	if again, _ := s.GetSwarmRequest(ctx, "s", node.ID); !reflect.DeepEqual(stored, again) || !reflect.DeepEqual(steps, stepStates(t, s)) {
		t.Fatalf("retry wrote: %+v", again)
	}
	for name, c := range map[string]struct{ user, outcome, response string }{
		"other manager": {"u", "success", "ship it"},
		"other text":    {"ed", "success", "different"},
		"no text":       {"ed", "success", ""},
		"other outcome": {"ed", "fail", "ship it"},
	} {
		if err := answer(s, node, c.user, c.outcome, c.response); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Автор теряет права, отключается или теряет сессию: повтор отвергнут.
	deny := func(context.Context) bool { return false }
	retry := SwarmAnswer{SwarmID: "s", RequestID: node.ID, UserID: "ed", Outcome: "success", Response: "ship it"}
	if _, err := s.AnswerSwarmRequest(ctx, retry, deny); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("no session: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE swarm_members SET role='viewer' WHERE user_id='ed'`); err != nil {
		t.Fatal(err)
	}
	if err := answer(s, node, "ed", "success", "ship it"); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("demoted author: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE swarm_members SET role='editor' WHERE user_id='ed'; UPDATE users SET disabled=1 WHERE id='ed'`); err != nil {
		t.Fatal(err)
	}
	if err := answer(s, node, "ed", "success", "ship it"); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("disabled author: %v", err)
	}
	if after, _ := s.GetSwarmRequest(ctx, "s", node.ID); !reflect.DeepEqual(stored, after) {
		t.Fatalf("denied retries wrote: %+v", after)
	}
}

func TestActorAnswerRetryFollowsTheSameRule(t *testing.T) {
	s, _ := requestStore(t)
	req := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, req, "ed", "", "sqlite"))
	mustNil(t, answer(s, req, "ed", "", "sqlite"))
	if err := answer(s, req, "ed", "", "postgres"); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("changed text: %v", err)
	}
	if err := answer(s, req, "u", "", "sqlite"); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("other author: %v", err)
	}
}

func TestAnsweredRequestAlwaysNamesItsAuthor(t *testing.T) {
	s, node := requestStore(t)
	if _, err := s.db.Exec(`UPDATE swarm_requests SET state='answered',answered_at='now',outcome='success' WHERE id=?`, node.ID); err == nil {
		t.Fatal("answer without an author accepted by the schema")
	}
	mustNil(t, answer(s, node, "u", "success", ""))
	got, _ := s.GetSwarmRequest(context.Background(), "s", node.ID)
	if got.AnsweredBy != "u" {
		t.Fatalf("answered_by %q", got.AnsweredBy)
	}
}

// Откат: отказ записи ответа, продвижения, нового запроса.
func TestAnswerRollsBackOnEveryWriteFailure(t *testing.T) {
	const loop = `apiVersion: brood.dev/v1alpha1
name: loop
start: ask
exit: done
nodes:
  ask:
    type: human
    options:
      - {label: Again, outcome: fail}
      - {label: Done, outcome: success}
  done: {type: exit}
edges:
  - {from: ask, to: ask, label: Again}
  - {from: ask, to: done, label: Done}
goalGates: [done]
`
	for name, trigger := range map[string]string{
		"answer write":   `CREATE TRIGGER inject BEFORE UPDATE OF state ON swarm_requests WHEN NEW.state='answered' BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		"step write":     `CREATE TRIGGER inject BEFORE UPDATE ON swarm_steps BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		"next request":   `CREATE TRIGGER inject BEFORE INSERT ON swarm_requests BEGIN SELECT RAISE(ABORT, 'injected'); END`,
		"execution flip": `CREATE TRIGGER inject BEFORE UPDATE OF execution ON swarms BEGIN SELECT RAISE(ABORT, 'injected'); END`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := swarmTestStore(t)
			if _, err := s.db.Exec(`UPDATE swarms SET state='active' WHERE id='s'`); err != nil {
				t.Fatal(err)
			}
			mustNil(t, attachTestGraph(ctx, s, "s", loop))
			if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
				t.Fatal(err)
			}
			req := openNode(t, s)
			before := snapshotRequests(t, s)
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if err := answer(s, req, "u", "fail", "again"); err == nil {
				t.Fatal("answer survived an injected failure")
			}
			if after := snapshotRequests(t, s); !reflect.DeepEqual(before, after) {
				t.Fatalf("partial answer: %+v", after)
			}
			if !reflect.DeepEqual(stepStates(t, s), []string{"ask:waiting"}) {
				t.Fatalf("steps: %v", stepStates(t, s))
			}
		})
	}
}

func TestStepAppendRollsBackWhenItsRequestCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	s := swarmTestStore(t)
	mustNil(t, attachTestGraph(ctx, s, "s", requestGraph))
	if _, err := s.db.Exec(`CREATE TRIGGER inject BEFORE INSERT ON swarm_requests BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "ask", State: "waiting"}); err == nil {
		t.Fatal("step created without its request")
	}
	if steps, _ := s.SwarmSteps(ctx, "s"); len(steps) != 0 {
		t.Fatalf("orphan steps: %+v", steps)
	}
}

func TestAnswerAuthFailureWritesNothing(t *testing.T) {
	ctx := context.Background()
	s, node := requestStore(t)
	before := snapshotRequests(t, s)
	for _, authorize := range []func(context.Context) bool{nil, func(context.Context) bool { return false }} {
		_, err := s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "s", RequestID: node.ID, UserID: "u", Outcome: "success"}, authorize)
		if !errors.Is(err, ErrSwarmRequestDenied) {
			t.Fatalf("auth: %v", err)
		}
	}
	if after := snapshotRequests(t, s); !reflect.DeepEqual(before, after) || !reflect.DeepEqual(stepStates(t, s), []string{"ask:waiting"}) {
		t.Fatalf("denied answer wrote: %+v", after)
	}
}

// Обхода авторизации нет в боевом коде: ответ без пользователя существует
// только в *_test.
func TestNoAuthBypassAnswerInProduction(t *testing.T) {
	files, err := filepath.Glob("*.go")
	mustNil(t, err)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		mustNil(t, err)
		if strings.Contains(string(data), "AnswerPipelineVisit") {
			t.Errorf("%s still names AnswerPipelineVisit", f)
		}
	}
}
