package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/build"
	"gopkg.in/yaml.v3"
)

// FEATURE: Шаги исполнения принадлежат рою, прогон перестал быть объектом (#206).
// SPEC: Снимок графа привязывается к рою с личностью определения и источником,
// шаги нумеруются атомарно, ответы и исходы применяются только к текущему шагу,
// состояние исполнения роя отделено от состояния инфраструктуры.
// IMPLEMENTATION: swarm_steps с ключом (swarm_id, sequence), graph и execution
// в swarms, чистый движок переходов (#207) применяет результаты.
// RESULT: IMPLEMENTED — привязка, шаги, ответы и восстановление живут на рое.
// STATUS: DONE
const runGraph = `apiVersion: brood.dev/v1alpha1
name: test
start: ask
exit: done
nodes:
  ask:
    type: human
    options:
      - {label: yes, outcome: success}
  actor: {type: actor, role: build, prompt: work}
  done: {type: exit}
edges:
  - {from: ask, to: actor}
  - {from: actor, to: done}
goalGates: [done]
`

// attachTestGraph привязывает граф рою напрямую, минуя обязательный
// производственный вход AttachSwarmGraph с его источником: движковые
// тесты проверяют шаги, а не fail-closed привязку источника. Снимок
// источника добавляется отдельным INSERT, когда тесту он нужен.
func attachTestGraph(ctx context.Context, s *Store, swarmID, graph string) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE swarms SET graph=?,execution='running',definition_name='test',definition_version=1,definition_revision='deadbeef',updated_at='now' WHERE id=?`, []byte(graph), swarmID)
	return err
}

func bindTestSource(ctx context.Context, s *Store, swarmID string) error {
	_, err := s.conn(ctx).ExecContext(ctx, `INSERT INTO pipeline_sources(swarm_id,origin,commit_sha) VALUES (?,?,?)`, swarmID, "https://git.example.org/repo", strings.Repeat("a", 40))
	return err
}

func swarmTestStore(t *testing.T, swarms ...string) *Store {
	t.Helper()
	s := newTestStore(t)
	if _, err := s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now'); INSERT INTO swarm_members VALUES ('s','u','owner')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range append([]string{"s"}, swarms...) {
		var exists bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM swarms WHERE id=?)`, id).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			continue
		}
		if _, err := s.db.Exec(`INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES (?, 'p', 'u', ?, ?, 'now', 'now')`, id, id, "space-"+id); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// BUG: A failed decision routed directly to exit succeeds.
// HYPOTHESIS: Exit progression ignores the preceding outcome.
// SOLUTION: Preserve a failed outcome when its selected successor is exit.
// RESULT: FIXED — direct failures fail while successful recovery can complete.
// STATUS: FIXED
// FEATURE: Execute the actual example's human decision.
// SPEC: Both answers reach the designated exit; actor failure fails closed.
// IMPLEMENTATION: Run the parsed example through the store using isolated state.
// RESULT: IMPLEMENTED — both answers are recorded and actor failure stops the graph.
// STATUS: DONE
// exampleGraph снимает граф разработки из examples/Broodfile: живой
// документ репозитория вместо рукописной копии.
func exampleGraph(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../examples/Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := build.ParseDocument(string(data), "examples/Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	graph, err := build.Workflow(doc, "inspect", "examples/Broodfile")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := yaml.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestExampleWorkflowRoutes(t *testing.T) {
	s := swarmTestStore(t, "success", "fail", "actor-fail")
	ctx := context.Background()
	for _, outcome := range []string{"success", "fail", "actor-fail"} {
		t.Run(outcome, func(t *testing.T) {
			if err := attachTestGraph(ctx, s, outcome, string(exampleGraph(t))); err != nil {
				t.Fatal(err)
			}
			actor := roleActor(t, s, outcome, "a-"+outcome, "inspect")
			if _, err := s.AppendPipelineVisit(ctx, outcome, outcome, PipelineVisit{NodeID: "inspect", State: "inflight", ActorUID: actor.UID, ActorIncarnation: 1, RequestID: "req"}); err != nil {
				t.Fatal(err)
			}
			actorOutcome := "success"
			if outcome == "actor-fail" {
				actorOutcome = "fail"
			}
			if err := s.CompletePipelineActorVisit(ctx, outcome, outcome, 1, "req", actor.UID, 1, actorOutcome, ""); err != nil {
				t.Fatal(err)
			}
			if outcome == "actor-fail" {
				sw, err := s.GetSwarm(ctx, outcome)
				if err != nil || sw.Execution != "failed" {
					t.Fatalf("swarm execution: %q %v", sw.Execution, err)
				}
				return
			}
			visits, err := s.SwarmSteps(ctx, outcome)
			if err != nil || len(visits) != 2 || visits[1].NodeID != "review" || visits[1].State != "waiting" {
				t.Fatalf("visits: %+v %v", visits, err)
			}
			if err := s.AnswerPipelineVisit(ctx, outcome, outcome, 2, outcome); err != nil {
				t.Fatal(err)
			}
			visits, err = s.SwarmSteps(ctx, outcome)
			if err != nil || len(visits) != 3 || visits[2].NodeID != "done" || visits[1].Answer == nil || *visits[1].Answer != outcome {
				t.Fatalf("visits: %+v %v", visits, err)
			}
			sw, err := s.GetSwarm(ctx, outcome)
			want := "succeeded"
			if outcome == "fail" {
				want = "failed"
			}
			if err != nil || sw.Execution != want {
				t.Fatalf("execution: got %q, want %q: %v", sw.Execution, want, err)
			}
		})
	}
}

// TestPipelineFailureRoutes exercises explicit exit and recovery edges through the store.
func TestPipelineFailureRoutes(t *testing.T) {
	const graph = `apiVersion: brood.dev/v1alpha1
name: routes
start: ask
exit: done
nodes:
  ask:
    type: human
    options:
      - {label: Reject, outcome: fail}
  repair: {type: actor, role: build, prompt: repair}
  done: {type: exit}
edges:
  - {from: ask, to: repair, label: Reject}
  - {from: repair, to: done, condition: "outcome=success"}
  - {from: repair, to: done, condition: "outcome=fail"}
goalGates: [done]
`
	for _, tc := range []struct{ outcome, want string }{{"success", "succeeded"}, {"fail", "failed"}} {
		t.Run(tc.outcome, func(t *testing.T) {
			s := swarmTestStore(t, "r")
			ctx := context.Background()
			if err := attachTestGraph(ctx, s, "r", graph); err != nil {
				t.Fatal(err)
			}
			roleActor(t, s, "r", "a", "build")
			if _, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
				t.Fatal(err)
			}
			if err := s.AnswerPipelineVisit(ctx, "r", "r", 1, "fail"); err != nil {
				t.Fatal(err)
			}
			visits, err := s.SwarmSteps(ctx, "r")
			if err != nil || len(visits) != 2 || visits[1].NodeID != "repair" || visits[1].State != "pending" {
				t.Fatalf("recovery visit: %+v %v", visits, err)
			}
			if _, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "repair", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: "req"}); err != nil {
				t.Fatal(err)
			}
			if err := s.CompletePipelineActorVisit(ctx, "r", "r", 2, "req", "a", 1, tc.outcome, ""); err != nil {
				t.Fatal(err)
			}
			visits, err = s.SwarmSteps(ctx, "r")
			if err != nil || len(visits) != 3 || visits[2].NodeID != "done" {
				t.Fatalf("exit visit: %+v %v", visits, err)
			}
			sw, err := s.GetSwarm(ctx, "r")
			if err != nil || sw.Execution != tc.want {
				t.Fatalf("execution: %q, want %s: %v", sw.Execution, tc.want, err)
			}
		})
	}
}

// Actor-start graphs exercise reservation independently of a human predecessor.
const actorStartGraph = `apiVersion: brood.dev/v1alpha1
name: actor-start
start: actor
exit: done
nodes:
  actor: {type: actor, role: build, prompt: work}
  done: {type: exit}
edges:
  - {from: actor, to: done}
goalGates: [done]
`

// BUG: Swarms accept graphs that describe actor provisioning instead of roles.
// HYPOTHESIS: Legacy template and managedPool fields fall out of validation.
// SOLUTION: Strict parsing rejects replaced fields with the role named (#208).
// RESULT: FIXED — legacy provisioning fields are rejected without persistence.
// STATUS: FIXED
func TestPipelineRejectsLegacyActorSources(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, graph string }{
		{"managed pool", strings.Replace(actorStartGraph, "role: build", "managedPool: pool", 1)},
		{"template", strings.Replace(actorStartGraph, "role: build", "template: development", 1)},
		{"missing role", strings.Replace(actorStartGraph, "role: build, ", "", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := swarmTestStore(t, "s")
			err := s.AttachSwarmGraph(ctx, "s", []byte(tc.graph), "test", 1, "rev", &PipelineSource{Origin: "https://example.com/group/repo.git", Commit: strings.Repeat("a", 40)})
			if err == nil || !strings.Contains(err.Error(), "role") {
				t.Errorf("legacy actor source accepted or refused without naming the role: %v", err)
			}
			sw, err := s.GetSwarm(ctx, "s")
			if err != nil || sw.Graph != nil || sw.Execution != "idle" {
				t.Errorf("legacy graph was persisted: %+v %v", sw, err)
			}
		})
	}
}

// BUG: Pipeline routing can lose failures or choose an unrelated option or exit.
// HYPOTHESIS: Generic edge selection falls back without validating execution semantics.
// SOLUTION: Validate routes and restrict selection, persisting unmatched outcomes as failure.
// RESULT: FIXED — failure and answer routing remain atomic and fail closed.
// STATUS: FIXED
func TestPipelineFailClosedRouting(t *testing.T) {
	s := swarmTestStore(t, "unconditional", "unmatched", "yes", "fail")
	ctx := context.Background()
	for _, tc := range []struct{ name, graph string }{
		{"unconditional", actorStartGraph},
		{"unmatched", strings.Replace(actorStartGraph, "from: actor, to: done}", "from: actor, to: done, condition: \"outcome=success\"}", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := attachTestGraph(ctx, s, tc.name, tc.graph); err != nil {
				t.Fatal(err)
			}
			actor := roleActor(t, s, tc.name, "a-"+tc.name, "build")
			if _, err := s.AppendPipelineVisit(ctx, tc.name, tc.name, PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: actor.UID, ActorIncarnation: 1, RequestID: "req"}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := s.CompletePipelineActorVisit(ctx, tc.name, tc.name, 1, "req", actor.UID, 1, "fail", ""); err != nil {
					t.Fatalf("completion/replay: %v", err)
				}
			}
			sw, err := s.GetSwarm(ctx, tc.name)
			if err != nil || sw.Execution != "failed" {
				t.Errorf("execution: %q %v", sw.Execution, err)
			}
			visits, err := s.SwarmSteps(ctx, tc.name)
			if err != nil || len(visits) != 1 || visits[0].State != "completed" || visits[0].Outcome == nil || *visits[0].Outcome != "fail" {
				t.Errorf("visits: %+v %v", visits, err)
			}
		})
	}
	t.Run("human option cannot fall through", func(t *testing.T) {
		s2 := swarmTestStore(t, "s")
		graph := strings.Replace(runGraph, "- {label: yes, outcome: success}", "- {label: yes, outcome: success}\n      - {label: no, outcome: fail}", 1)
		graph = strings.Replace(graph, "- {from: ask, to: actor}", "- {from: ask, to: actor, label: no}", 1)
		if err := s2.AttachSwarmGraph(ctx, "s", []byte(graph), "test", 1, "rev", &PipelineSource{Origin: "https://git.example.org/repo", Commit: strings.Repeat("a", 40)}); err == nil {
			t.Error("accepted human option with no matching route")
		}
	})
	t.Run("human exact route", func(t *testing.T) {
		s2 := swarmTestStore(t, "success", "fail")
		graph := strings.Replace(runGraph, "- {label: yes, outcome: success}", "- {label: yes, outcome: success}\n      - {label: no, outcome: fail}", 1)
		graph = strings.Replace(graph, "- {from: ask, to: actor}", "- {from: ask, to: actor, label: yes}\n  - {from: ask, to: done, label: no}", 1)
		for _, tc := range []struct{ answer, next string }{{"success", "actor"}, {"fail", "done"}} {
			if err := attachTestGraph(ctx, s2, tc.answer, graph); err != nil {
				t.Fatal(err)
			}
			if _, err := s2.AppendPipelineVisit(ctx, tc.answer, tc.answer, PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
				t.Fatal(err)
			}
			if err := s2.AnswerPipelineVisit(ctx, tc.answer, tc.answer, 1, tc.answer); err != nil {
				t.Fatal(err)
			}
			visits, err := s2.SwarmSteps(ctx, tc.answer)
			if err != nil || len(visits) != 2 || visits[1].NodeID != tc.next {
				t.Fatalf("answer %s: %+v %v", tc.answer, visits, err)
			}
		}
	})
	t.Run("other exit", func(t *testing.T) {
		s2 := swarmTestStore(t, "s")
		graph := strings.Replace(actorStartGraph, "  done: {type: exit}", "  done: {type: exit}\n  other: {type: exit}", 1)
		graph = strings.Replace(graph, "from: actor, to: done}", "from: actor, to: other}", 1)
		if err := s2.AttachSwarmGraph(ctx, "s", []byte(graph), "test", 1, "rev", &PipelineSource{Origin: "https://git.example.org/repo", Commit: strings.Repeat("a", 40)}); err == nil {
			t.Error("accepted non-designated exit")
		}
	})
}

func TestPipelineDistinctActorReservations(t *testing.T) {
	s := swarmTestStore(t, "r")
	ctx := context.Background()
	if err := attachTestGraph(ctx, s, "r", actorStartGraph); err != nil {
		t.Fatal(err)
	}
	roleActor(t, s, "r", "a", "build")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, request := range []string{"one", "two"} {
		wg.Add(1)
		go func(request string) {
			defer wg.Done()
			_, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: request})
			results <- err
		}(request)
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("got %d reservations, want 1", wins)
	}
	if _, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err == nil {
		t.Fatal("unresolved actor visit was superseded")
	}
}

// BUG: Incomplete inflight visits and unknown nodes are accepted.
// HYPOTHESIS: Append validates field lengths but not the graph or state metadata.
// SOLUTION: Validate the node type and allowed initial state and metadata.
// RESULT: FIXED — invalid visits are rejected.
// STATUS: FIXED
func TestPipelineVisitValidation(t *testing.T) {
	s := swarmTestStore(t, "r")
	ctx := context.Background()
	if err := attachTestGraph(ctx, s, "r", actorStartGraph); err != nil {
		t.Fatal(err)
	}
	// Роль привязана к «a»: отказ даёт проверка полей, а не привязка.
	roleActor(t, s, "r", "a", "build")
	for _, v := range []PipelineVisit{{NodeID: "missing", State: "waiting"}, {NodeID: "actor", State: "inflight"}, {NodeID: "actor", State: "inflight", ActorUID: "a", RequestID: "x"}, {NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: -1, RequestID: "x"}, {NodeID: "ask", State: "waiting", ActorUID: "actor"}, {NodeID: "ask", State: "invented"}} {
		if _, err := s.AppendPipelineVisit(ctx, "r", "r", v); err == nil {
			t.Errorf("accepted invalid visit: %+v", v)
		}
	}
}

// BUG: Duplicate action requests create multiple visits and recovery skips incomplete rows.
// HYPOTHESIS: Request IDs lack uniqueness and recovery filters on optional metadata.
// SOLUTION: Reserve request IDs uniquely, check payloads, and recover all inflight rows.
// RESULT: FIXED — replays cannot reserve again and incomplete rows become unconfirmed.
// STATUS: FIXED
func TestPipelineReservationAndRecovery(t *testing.T) {
	s := swarmTestStore(t, "r")
	ctx := context.Background()
	if err := attachTestGraph(ctx, s, "r", actorStartGraph); err != nil {
		t.Fatal(err)
	}
	roleActor(t, s, "r", "a", "build")
	v := PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: "key"}
	const n = 10
	var wg sync.WaitGroup
	results := make(chan PipelineVisit, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := s.AppendPipelineVisit(ctx, "r", "r", v)
			if e != nil {
				errs <- e
			} else {
				results <- got
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	newReservations := 0
	for got := range results {
		if got.Sequence != 1 {
			t.Errorf("duplicate reservation sequence %d", got.Sequence)
		}
		if !got.Replay {
			newReservations++
		}
	}
	if newReservations != 1 {
		t.Errorf("new reservations: got %d, want 1", newReservations)
	}
	v.ActorUID = "different"
	if _, err := s.AppendPipelineVisit(ctx, "r", "r", v); err == nil {
		t.Error("mismatched request accepted")
	}
	_, err := s.db.Exec(`INSERT INTO swarm_steps (swarm_id,sequence,node_id,state) VALUES ('r',100,'actor','inflight')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverPipelineActions(ctx); err != nil {
		t.Fatal(err)
	}
	v.ActorUID = "a"
	replayed, err := s.AppendPipelineVisit(ctx, "r", "r", v)
	if err != nil || !replayed.Replay || replayed.State != "unconfirmed" {
		t.Errorf("replay after recovery: %+v %v", replayed, err)
	}
	var state string
	if err = s.db.QueryRow(`SELECT state FROM swarm_steps WHERE swarm_id='r' AND sequence=100`).Scan(&state); err != nil || state != "unconfirmed" {
		t.Errorf("incomplete recovery: %q %v", state, err)
	}
}

// FEATURE: Complete an actor visit with its authenticated reservation identity.
// SPEC: Only the latest inflight visit of an active swarm graph accepts a new outcome;
// exact retries are idempotent, and conflicting callbacks are rejected.
// IMPLEMENTATION: Conditionally persist the outcome in one transaction.
// RESULT: IMPLEMENTED — identity, state, recency and swarm execution are checked atomically.
// STATUS: DONE
func TestCompletePipelineActorVisit(t *testing.T) {
	s := swarmTestStore(t, "r", "uncertain", "terminal", "concurrent")
	ctx := context.Background()
	// newSwarm — рой с актором «a-<рой>» на инкарнации 2, привязанным к роли
	// узла, и его шагом inflight.
	newSwarm := func(id string) {
		t.Helper()
		if err := attachTestGraph(ctx, s, id, actorStartGraph); err != nil {
			t.Fatal(err)
		}
		roleActor(t, s, id, "a-"+id, "")
		rebindAt(t, s, id, "a-"+id, "build", 2)
		if _, err := s.AppendPipelineVisit(ctx, id, id, PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a-" + id, ActorIncarnation: 2, RequestID: "req"}); err != nil {
			t.Fatal(err)
		}
	}
	newSwarm("r")
	complete := func(swarm, run string, seq int64, request, actor string, incarnation int64, outcome string) error {
		return s.CompletePipelineActorVisit(ctx, swarm, run, seq, request, actor, incarnation, outcome, "")
	}
	for _, tc := range []struct {
		swarm, run     string
		seq            int64
		request, actor string
		incarnation    int64
		outcome        string
	}{
		{"missing", "r", 1, "req", "a-r", 2, "success"}, {"r", "r", 2, "req", "a-r", 2, "success"},
		{"r", "r", 1, "fake", "a-r", 2, "success"},
		{"r", "r", 1, "req", "fake", 2, "success"}, {"r", "r", 1, "req", "a-r", 3, "success"},
		{"r", "r", 1, "req", "a-r", 2, "invalid"}, {"r", "r", 0, "req", "a-r", 2, "success"},
	} {
		if err := complete(tc.swarm, tc.run, tc.seq, tc.request, tc.actor, tc.incarnation, tc.outcome); err == nil {
			t.Errorf("accepted forged callback: %+v", tc)
		}
	}
	if err := complete("r", "r", 1, "req", "a-r", 2, "success"); err != nil {
		t.Fatal(err)
	}
	if err := complete("r", "r", 1, "req", "a-r", 2, "success"); err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if err := complete("r", "r", 1, "req", "a-r", 2, "fail"); err == nil {
		t.Fatal("accepted mismatched outcome")
	}
	visits, err := s.SwarmSteps(ctx, "r")
	if err != nil || len(visits) != 2 || visits[0].State != "completed" || visits[0].Outcome == nil || *visits[0].Outcome != "success" || visits[1].NodeID != "done" || visits[1].State != "completed" {
		t.Fatalf("visit: %+v %v", visits, err)
	}
	if err := complete("r", "r", 1, "req", "a-r", 2, "success"); err != nil {
		t.Fatalf("retry after progression: %v", err)
	}
	replay, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a-r", ActorIncarnation: 2, RequestID: "req"})
	if err != nil || !replay.Replay || replay.Sequence != 1 || replay.State != "completed" {
		t.Fatalf("reservation replay after completion: %+v %v", replay, err)
	}
	newSwarm("uncertain")
	if err := s.RecoverPipelineActions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := complete("uncertain", "uncertain", 1, "req", "a-uncertain", 2, "success"); err == nil {
		t.Fatal("accepted unconfirmed work")
	}
	newSwarm("terminal")
	if _, err := s.db.Exec(`UPDATE swarms SET execution='succeeded' WHERE id='terminal'`); err != nil {
		t.Fatal(err)
	}
	if err := complete("terminal", "terminal", 1, "req", "a-terminal", 2, "success"); err == nil {
		t.Fatal("accepted terminal graph")
	}
	newSwarm("concurrent")
	const m = 12
	var wg sync.WaitGroup
	results := make(chan error, m)
	for i := 0; i < m; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- complete("concurrent", "concurrent", 1, "req", "a-concurrent", 2, "fail")
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent retry: %v", err)
		}
	}
	visits, err = s.SwarmSteps(ctx, "concurrent")
	if err != nil || len(visits) != 1 || visits[0].Outcome == nil || *visits[0].Outcome != "fail" {
		t.Fatalf("concurrent outcome: %+v %v", visits, err)
	}
}

// FEATURE: Atomic graph progression.
// SPEC: Only the start may be appended; answering creates one successor and exact retries succeed.
func TestPipelineAtomicProgression(t *testing.T) {
	s := swarmTestStore(t, "r")
	ctx := context.Background()
	if err := attachTestGraph(ctx, s, "r", runGraph); err != nil {
		t.Fatal(err)
	}
	// Роль привязана к «a»: отказ первому шагу даёт нестартовый узел.
	roleActor(t, s, "r", "a", "build")
	if _, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: "x"}); err == nil {
		t.Fatal("accepted non-start node")
	}
	if _, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err == nil {
		t.Fatal("accepted second current visit")
	}
	if err := s.AnswerPipelineVisit(ctx, "r", "r", 1, "success"); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerPipelineVisit(ctx, "r", "r", 1, "success"); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	visits, err := s.SwarmSteps(ctx, "r")
	if err != nil || len(visits) != 2 || visits[1].NodeID != "actor" {
		t.Fatalf("successor: %+v %v", visits, err)
	}
}

func TestPipelineProgressionRollbackAndLoop(t *testing.T) {
	s := swarmTestStore(t, "r")
	ctx := context.Background()
	graph := strings.Replace(runGraph, "  - {from: actor, to: done}", "  - {from: actor, to: done, condition: \"outcome=success\"}\n  - {from: actor, to: actor, condition: \"outcome=fail\"}", 1)
	if err := attachTestGraph(ctx, s, "r", graph); err != nil {
		t.Fatal(err)
	}
	roleActor(t, s, "r", "a", "build")
	if _, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_successor BEFORE INSERT ON swarm_steps WHEN NEW.sequence=2 BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerPipelineVisit(ctx, "r", "r", 1, "success"); err == nil {
		t.Fatal("injected failure accepted")
	}
	visits, err := s.SwarmSteps(ctx, "r")
	if err != nil || len(visits) != 1 || visits[0].State != "waiting" || visits[0].Answer != nil {
		t.Fatalf("partial progression after failure: %+v %v", visits, err)
	}
	sw, err := s.GetSwarm(ctx, "r")
	if err != nil || sw.Execution != "running" {
		t.Fatalf("graph after rollback: %q %v", sw.Execution, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_successor`); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerPipelineVisit(ctx, "r", "r", 1, "success"); err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 2; i++ {
		seq := 2 + i
		request := []string{"first", "second"}[i]
		v, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: request})
		if err != nil || v.Sequence != seq || v.Replay {
			t.Fatalf("reservation: %+v %v", v, err)
		}
		outcome := "fail"
		if i == 1 {
			outcome = "success"
		}
		if err := s.CompletePipelineActorVisit(ctx, "r", "r", seq, request, "a", 1, outcome, ""); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			replay, err := s.AppendPipelineVisit(ctx, "r", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: request})
			if err != nil || !replay.Replay || replay.Sequence != seq || replay.State != "completed" {
				t.Fatalf("replay must not reserve loop successor: %+v %v", replay, err)
			}
		}
	}
	visits, err = s.SwarmSteps(ctx, "r")
	if err != nil || len(visits) != 4 || visits[1].NodeID != "actor" || visits[2].NodeID != "actor" || visits[3].NodeID != "done" || visits[3].State != "completed" {
		t.Fatalf("loop visits: %+v %v", visits, err)
	}
	sw, err = s.GetSwarm(ctx, "r")
	if err != nil || sw.Execution != "succeeded" {
		t.Fatalf("finished graph: %q %v", sw.Execution, err)
	}
}

func TestSwarmStepsOperations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now'),('other','p','u','other','other','now','now'); INSERT INTO swarm_members VALUES ('s','u','owner'),('other','u','owner')`)
	if err != nil {
		t.Fatal(err)
	}
	for _, graph := range []string{"", "garbage", strings.Repeat("x", MaxPipelineGraphBytes+1), strings.Replace(runGraph, "exit: done", "exit: missing", 1)} {
		if err := s.AttachSwarmGraph(ctx, "s", []byte(graph), "test", 1, "rev", &PipelineSource{Origin: "https://git.example.org/repo", Commit: strings.Repeat("a", 40)}); err == nil {
			t.Fatal("invalid graph accepted")
		}
	}
	sw, err := s.GetSwarm(ctx, "s")
	if err != nil || sw.Graph != nil {
		t.Fatalf("invalid graph persisted: %+v %v", sw, err)
	}
	if err := attachTestGraph(ctx, s, "s", runGraph); err != nil {
		t.Fatal(err)
	}
	const n = 12
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "ask", State: "waiting"})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrPipelineVisitConflict) {
			t.Errorf("append: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("start winners: %d", wins)
	}
	results = make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.AnswerPipelineVisit(ctx, "s", "s", 1, "success") }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("answer retry: %v", err)
		}
	}
	visits, err := s.SwarmSteps(ctx, "s")
	if err != nil || len(visits) != 2 || visits[0].State != "answered" || visits[1].NodeID != "actor" || visits[1].State != "pending" {
		t.Fatalf("concurrent progression: %+v %v", visits, err)
	}
	if _, err := s.AppendPipelineVisit(ctx, "other", "other", PipelineVisit{NodeID: "ask", State: "waiting"}); err == nil {
		t.Fatal("graphless swarm accepted a visit")
	}
	roleActor(t, s, "s", "actor", "")
	rebindAt(t, s, "s", "actor", "build", 2)
	if _, err = s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "actor", ActorIncarnation: 2, RequestID: "req"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RecoverPipelineActions(ctx); err != nil {
		t.Fatal(err)
	}
	visits, err = s.SwarmSteps(ctx, "s")
	if err != nil || len(visits) != 2 || visits[len(visits)-1].State != "unconfirmed" || visits[len(visits)-1].RequestID != "req" {
		t.Fatalf("recovery: %+v %v", visits, err)
	}
}

// BUG: Terminal graphs accept visits and answers.
// HYPOTHESIS: Visit operations never inspect the swarm execution.
// SOLUTION: Check active execution within the write transaction.
// RESULT: FIXED — terminal graphs reject both mutations.
// STATUS: FIXED
func TestPipelineTerminalGraphRejectsVisits(t *testing.T) {
	s := swarmTestStore(t, "succeeded", "failed", "stopped")
	ctx := context.Background()
	for _, execution := range []string{"succeeded", "failed", "stopped"} {
		t.Run(execution, func(t *testing.T) {
			if err := attachTestGraph(ctx, s, execution, runGraph); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendPipelineVisit(ctx, execution, execution, PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE swarms SET execution=? WHERE id=?`, execution, execution); err != nil {
				t.Fatal(err)
			}
			if err := s.AnswerPipelineVisit(ctx, execution, execution, 1, "success"); err == nil {
				t.Error("terminal graph accepted answer")
			}
			if _, err := s.AppendPipelineVisit(ctx, execution, execution, PipelineVisit{NodeID: "ask", State: "waiting"}); err == nil {
				t.Error("terminal graph accepted visit")
			}
		})
	}
}
