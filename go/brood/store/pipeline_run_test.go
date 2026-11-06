package store

import (
	"context"
	"errors"
	"os"

	"brood.dev/go/brood/build"
	"gopkg.in/yaml.v3"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// FEATURE: Transactional pipeline run storage.
// SPEC: Runs are swarm-scoped, snapshots validated, visits numbered atomically,
// answers apply only to the current waiting visit, and interrupted actions stay unconfirmed.
// IMPLEMENTATION: Use atomic SQL writes and validate snapshots before insertion.
// RESULT: IMPLEMENTED — scoped runs, visits, answers, and recovery are persisted.
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
  actor: {type: actor, template: development, prompt: work}
  done: {type: exit}
edges:
  - {from: ask, to: actor}
  - {from: actor, to: done}
goalGates: [done]
`

// BUG: A failed decision routed directly to exit succeeds.
// HYPOTHESIS: Exit progression ignores the preceding outcome.
// SOLUTION: Preserve a failed outcome when its selected successor is exit.
// RESULT: FIXED — direct failures fail while successful recovery can complete.
// STATUS: FIXED
// FEATURE: Execute the actual example's human decision.
// SPEC: Both answers reach the designated exit; actor failure fails closed.
// IMPLEMENTATION: Run the parsed example through the store using isolated state.
// RESULT: IMPLEMENTED — both answers are recorded and actor failure stops the run.
// STATUS: DONE
func TestExampleWorkflowRoutes(t *testing.T) {
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
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now')`)
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"success", "fail", "actor-fail"} {
		t.Run(outcome, func(t *testing.T) {
			if err := s.CreatePipelineRun(ctx, PipelineRun{ID: outcome, SwarmID: "s", OwnerID: "u", Graph: snapshot, Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendPipelineVisit(ctx, "s", outcome, PipelineVisit{NodeID: "inspect", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: "req"}); err != nil {
				t.Fatal(err)
			}
			actorOutcome := "success"
			if outcome == "actor-fail" {
				actorOutcome = "fail"
			}
			if err := s.CompletePipelineActorVisit(ctx, "s", outcome, 1, "req", "a", 1, actorOutcome); err != nil {
				t.Fatal(err)
			}
			if outcome == "actor-fail" {
				run, err := s.GetPipelineRun(ctx, "s", outcome)
				if err != nil || run.Status != "failed" {
					t.Fatalf("run: %+v %v", run, err)
				}
				return
			}
			visits, err := s.ListPipelineVisits(ctx, "s", outcome)
			if err != nil || len(visits) != 2 || visits[1].NodeID != "review" || visits[1].State != "waiting" {
				t.Fatalf("visits: %+v %v", visits, err)
			}
			if err := s.AnswerPipelineVisit(ctx, "s", outcome, 2, outcome); err != nil {
				t.Fatal(err)
			}
			visits, err = s.ListPipelineVisits(ctx, "s", outcome)
			if err != nil || len(visits) != 3 || visits[2].NodeID != "done" || visits[1].Answer == nil || *visits[1].Answer != outcome {
				t.Fatalf("visits: %+v %v", visits, err)
			}
			run, err := s.GetPipelineRun(ctx, "s", outcome)
			want := "succeeded"
			if outcome == "fail" {
				want = "failed"
			}
			if err != nil || run.Status != want {
				t.Fatalf("run status: got %q, want %q: %v", run.Status, want, err)
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
  repair: {type: actor, template: development, prompt: repair}
  done: {type: exit}
edges:
  - {from: ask, to: repair, label: Reject}
  - {from: repair, to: done, condition: "outcome=success"}
  - {from: repair, to: done, condition: "outcome=fail"}
goalGates: [done]
`
	for _, tc := range []struct{ outcome, want string }{{"success", "succeeded"}, {"fail", "failed"}} {
		t.Run(tc.outcome, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now')`)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.CreatePipelineRun(ctx, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: []byte(graph), Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
				t.Fatal(err)
			}
			if err := s.AnswerPipelineVisit(ctx, "s", "r", 1, "fail"); err != nil {
				t.Fatal(err)
			}
			visits, err := s.ListPipelineVisits(ctx, "s", "r")
			if err != nil || len(visits) != 2 || visits[1].NodeID != "repair" || visits[1].State != "pending" {
				t.Fatalf("recovery visit: %+v %v", visits, err)
			}
			if _, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "repair", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: "req"}); err != nil {
				t.Fatal(err)
			}
			if err := s.CompletePipelineActorVisit(ctx, "s", "r", 2, "req", "a", 1, tc.outcome); err != nil {
				t.Fatal(err)
			}
			visits, err = s.ListPipelineVisits(ctx, "s", "r")
			if err != nil || len(visits) != 3 || visits[2].NodeID != "done" {
				t.Fatalf("exit visit: %+v %v", visits, err)
			}
			run, err := s.GetPipelineRun(ctx, "s", "r")
			if err != nil || run.Status != tc.want {
				t.Fatalf("run: %+v, want %s: %v", run, tc.want, err)
			}
		})
	}
}

// Actor-start runs exercise reservation independently of a human predecessor.
const actorStartGraph = `apiVersion: brood.dev/v1alpha1
name: actor-start
start: actor
exit: done
nodes:
  actor: {type: actor, template: development, prompt: work}
  done: {type: exit}
edges:
  - {from: actor, to: done}
goalGates: [done]
`

// BUG: Pipeline routing can lose failures or choose an unrelated option or exit.
// HYPOTHESIS: Generic edge selection falls back without validating execution semantics.
// SOLUTION: Validate routes and restrict selection, persisting unmatched outcomes as failure.
// RESULT: FIXED — failure and answer routing remain atomic and fail closed.
// STATUS: FIXED
func TestPipelineFailClosedRouting(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now')`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, graph string }{
		{"unconditional", actorStartGraph},
		{"unmatched", strings.Replace(actorStartGraph, "from: actor, to: done}", "from: actor, to: done, condition: \"outcome=success\"}", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.CreatePipelineRun(ctx, PipelineRun{ID: tc.name, SwarmID: "s", OwnerID: "u", Graph: []byte(tc.graph), Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendPipelineVisit(ctx, "s", tc.name, PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: "req"}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := s.CompletePipelineActorVisit(ctx, "s", tc.name, 1, "req", "a", 1, "fail"); err != nil {
					t.Fatalf("completion/replay: %v", err)
				}
			}
			run, err := s.GetPipelineRun(ctx, "s", tc.name)
			if err != nil || run.Status != "failed" {
				t.Errorf("run status: %+v %v", run, err)
			}
			visits, err := s.ListPipelineVisits(ctx, "s", tc.name)
			if err != nil || len(visits) != 1 || visits[0].State != "completed" || visits[0].Outcome == nil || *visits[0].Outcome != "fail" {
				t.Errorf("visits: %+v %v", visits, err)
			}
		})
	}
	t.Run("human option cannot fall through", func(t *testing.T) {
		graph := strings.Replace(runGraph, "- {label: yes, outcome: success}", "- {label: yes, outcome: success}\n      - {label: no, outcome: fail}", 1)
		graph = strings.Replace(graph, "- {from: ask, to: actor}", "- {from: ask, to: actor, label: no}", 1)
		if err := s.CreatePipelineRun(ctx, PipelineRun{ID: "human", SwarmID: "s", OwnerID: "u", Graph: []byte(graph), Status: "running"}); err == nil {
			t.Error("accepted human option with no matching route")
		}
	})
	t.Run("human exact route", func(t *testing.T) {
		graph := strings.Replace(runGraph, "- {label: yes, outcome: success}", "- {label: yes, outcome: success}\n      - {label: no, outcome: fail}", 1)
		graph = strings.Replace(graph, "- {from: ask, to: actor}", "- {from: ask, to: actor, label: yes}\n  - {from: ask, to: done, label: no}", 1)
		for _, tc := range []struct{ answer, next string }{{"success", "actor"}, {"fail", "done"}} {
			if err := s.CreatePipelineRun(ctx, PipelineRun{ID: tc.answer, SwarmID: "s", OwnerID: "u", Graph: []byte(graph), Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendPipelineVisit(ctx, "s", tc.answer, PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
				t.Fatal(err)
			}
			if err := s.AnswerPipelineVisit(ctx, "s", tc.answer, 1, tc.answer); err != nil {
				t.Fatal(err)
			}
			visits, err := s.ListPipelineVisits(ctx, "s", tc.answer)
			if err != nil || len(visits) != 2 || visits[1].NodeID != tc.next {
				t.Fatalf("answer %s: %+v %v", tc.answer, visits, err)
			}
		}
	})
	t.Run("other exit", func(t *testing.T) {
		graph := strings.Replace(actorStartGraph, "  done: {type: exit}", "  done: {type: exit}\n  other: {type: exit}", 1)
		graph = strings.Replace(graph, "from: actor, to: done}", "from: actor, to: other}", 1)
		if err := s.CreatePipelineRun(ctx, PipelineRun{ID: "exit", SwarmID: "s", OwnerID: "u", Graph: []byte(graph), Status: "running"}); err == nil {
			t.Error("accepted non-designated exit")
		}
	})
}

func TestPipelineDistinctActorReservations(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreatePipelineRun(ctx, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: []byte(actorStartGraph), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, request := range []string{"one", "two"} {
		wg.Add(1)
		go func(request string) {
			defer wg.Done()
			_, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: request})
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
	if _, err = s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err == nil {
		t.Fatal("unresolved actor visit was superseded")
	}
}

// BUG: Incomplete inflight visits and unknown nodes are accepted.
// HYPOTHESIS: Append validates field lengths but not the graph or state metadata.
// SOLUTION: Validate the node type and allowed initial state and metadata.
// RESULT: FIXED — invalid visits are rejected.
// STATUS: FIXED
func TestPipelineVisitValidation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePipelineRun(ctx, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: []byte(actorStartGraph), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []PipelineVisit{{NodeID: "missing", State: "waiting"}, {NodeID: "actor", State: "inflight"}, {NodeID: "actor", State: "inflight", ActorUID: "a", RequestID: "x"}, {NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: -1, RequestID: "x"}, {NodeID: "ask", State: "waiting", ActorUID: "actor"}, {NodeID: "ask", State: "invented"}} {
		if _, err := s.AppendPipelineVisit(ctx, "s", "r", v); err == nil {
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
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePipelineRun(ctx, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: []byte(actorStartGraph), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	v := PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: "key"}
	const n = 10
	var wg sync.WaitGroup
	results := make(chan PipelineVisit, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := s.AppendPipelineVisit(ctx, "s", "r", v)
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
	if _, err := s.AppendPipelineVisit(ctx, "s", "r", v); err == nil {
		t.Error("mismatched request accepted")
	}
	_, err = s.db.Exec(`INSERT INTO pipeline_steps (run_id,sequence,node_id,state) VALUES ('r',100,'actor','inflight')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverPipelineActions(ctx); err != nil {
		t.Fatal(err)
	}
	v.ActorUID = "a"
	replayed, err := s.AppendPipelineVisit(ctx, "s", "r", v)
	if err != nil || !replayed.Replay || replayed.State != "unconfirmed" {
		t.Errorf("replay after recovery: %+v %v", replayed, err)
	}
	var state string
	if err = s.db.QueryRow(`SELECT state FROM pipeline_steps WHERE run_id='r' AND sequence=100`).Scan(&state); err != nil || state != "unconfirmed" {
		t.Errorf("incomplete recovery: %q %v", state, err)
	}
}

// FEATURE: Complete an actor visit with its authenticated reservation identity.
// SPEC: Only the latest inflight visit of an active run accepts a new outcome;
// exact retries are idempotent, and conflicting callbacks are rejected.
// IMPLEMENTATION: Conditionally persist the outcome in one transaction.
// RESULT: IMPLEMENTED — identity, state, recency and run status are checked atomically.
// STATUS: DONE
func TestCompletePipelineActorVisit(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now'),('other','p','u','other','other','now','now')`)
	if err != nil {
		t.Fatal(err)
	}
	newRun := func(id string) {
		t.Helper()
		if err := s.CreatePipelineRun(ctx, PipelineRun{ID: id, SwarmID: "s", OwnerID: "u", Graph: []byte(actorStartGraph), Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendPipelineVisit(ctx, "s", id, PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 2, RequestID: "req"}); err != nil {
			t.Fatal(err)
		}
	}
	newRun("r")
	complete := func(swarm, run string, seq int64, request, actor string, incarnation int64, outcome string) error {
		return s.CompletePipelineActorVisit(ctx, swarm, run, seq, request, actor, incarnation, outcome)
	}
	for _, tc := range []struct {
		swarm, run     string
		seq            int64
		request, actor string
		incarnation    int64
		outcome        string
	}{
		{"other", "r", 1, "req", "a", 2, "success"}, {"s", "missing", 1, "req", "a", 2, "success"},
		{"s", "r", 2, "req", "a", 2, "success"}, {"s", "r", 1, "fake", "a", 2, "success"},
		{"s", "r", 1, "req", "fake", 2, "success"}, {"s", "r", 1, "req", "a", 3, "success"},
		{"s", "r", 1, "req", "a", 2, "invalid"}, {"s", "r", 0, "req", "a", 2, "success"},
	} {
		if err := complete(tc.swarm, tc.run, tc.seq, tc.request, tc.actor, tc.incarnation, tc.outcome); err == nil {
			t.Errorf("accepted forged callback: %+v", tc)
		}
	}
	if err := complete("s", "r", 1, "req", "a", 2, "success"); err != nil {
		t.Fatal(err)
	}
	if err := complete("s", "r", 1, "req", "a", 2, "success"); err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if err := complete("s", "r", 1, "req", "a", 2, "fail"); err == nil {
		t.Fatal("accepted mismatched outcome")
	}
	visits, err := s.ListPipelineVisits(ctx, "s", "r")
	if err != nil || len(visits) != 2 || visits[0].State != "completed" || visits[0].Outcome == nil || *visits[0].Outcome != "success" || visits[1].NodeID != "done" || visits[1].State != "completed" {
		t.Fatalf("visit: %+v %v", visits, err)
	}
	if err := complete("s", "r", 1, "req", "a", 2, "success"); err != nil {
		t.Fatalf("retry after progression: %v", err)
	}
	replay, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 2, RequestID: "req"})
	if err != nil || !replay.Replay || replay.Sequence != 1 || replay.State != "completed" {
		t.Fatalf("reservation replay after completion: %+v %v", replay, err)
	}
	newRun("uncertain")
	if err := s.RecoverPipelineActions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := complete("s", "uncertain", 1, "req", "a", 2, "success"); err == nil {
		t.Fatal("accepted unconfirmed work")
	}
	newRun("terminal")
	if _, err := s.db.Exec(`UPDATE pipeline_runs SET status='succeeded' WHERE id='terminal'`); err != nil {
		t.Fatal(err)
	}
	if err := complete("s", "terminal", 1, "req", "a", 2, "success"); err == nil {
		t.Fatal("accepted terminal run")
	}
	newRun("concurrent")
	const n = 12
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- complete("s", "concurrent", 1, "req", "a", 2, "fail") }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent retry: %v", err)
		}
	}
	visits, err = s.ListPipelineVisits(ctx, "s", "concurrent")
	if err != nil || len(visits) != 1 || visits[0].Outcome == nil || *visits[0].Outcome != "fail" {
		t.Fatalf("concurrent outcome: %+v %v", visits, err)
	}
}

// FEATURE: Atomic pipeline progression.
// SPEC: Only the start may be appended; answering creates one successor and exact retries succeed.
func TestPipelineAtomicProgression(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreatePipelineRun(ctx, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: []byte(runGraph), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: "x"}); err == nil {
		t.Fatal("accepted non-start node")
	}
	if _, err = s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err == nil {
		t.Fatal("accepted second current visit")
	}
	if err = s.AnswerPipelineVisit(ctx, "s", "r", 1, "success"); err != nil {
		t.Fatal(err)
	}
	if err = s.AnswerPipelineVisit(ctx, "s", "r", 1, "success"); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	visits, err := s.ListPipelineVisits(ctx, "s", "r")
	if err != nil || len(visits) != 2 || visits[1].NodeID != "actor" {
		t.Fatalf("successor: %+v %v", visits, err)
	}
}

func TestPipelineProgressionRollbackAndLoop(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now')`)
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(runGraph, "  - {from: actor, to: done}", "  - {from: actor, to: done, condition: \"outcome=success\"}\n  - {from: actor, to: actor, condition: \"outcome=fail\"}", 1)
	if err := s.CreatePipelineRun(ctx, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: []byte(graph), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_successor BEFORE INSERT ON pipeline_steps WHEN NEW.sequence=2 BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerPipelineVisit(ctx, "s", "r", 1, "success"); err == nil {
		t.Fatal("injected failure accepted")
	}
	visits, err := s.ListPipelineVisits(ctx, "s", "r")
	if err != nil || len(visits) != 1 || visits[0].State != "waiting" || visits[0].Answer != nil {
		t.Fatalf("partial progression after failure: %+v %v", visits, err)
	}
	run, err := s.GetPipelineRun(ctx, "s", "r")
	if err != nil || run.Status != "running" {
		t.Fatalf("run after rollback: %+v %v", run, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_successor`); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerPipelineVisit(ctx, "s", "r", 1, "success"); err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 2; i++ {
		seq := 2 + i
		request := []string{"first", "second"}[i]
		v, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: request})
		if err != nil || v.Sequence != seq || v.Replay {
			t.Fatalf("reservation: %+v %v", v, err)
		}
		outcome := "fail"
		if i == 1 {
			outcome = "success"
		}
		if err := s.CompletePipelineActorVisit(ctx, "s", "r", seq, request, "a", 1, outcome); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			replay, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "a", ActorIncarnation: 1, RequestID: request})
			if err != nil || !replay.Replay || replay.Sequence != seq || replay.State != "completed" {
				t.Fatalf("replay must not reserve loop successor: %+v %v", replay, err)
			}
		}
	}
	visits, err = s.ListPipelineVisits(ctx, "s", "r")
	if err != nil || len(visits) != 4 || visits[1].NodeID != "actor" || visits[2].NodeID != "actor" || visits[3].NodeID != "done" || visits[3].State != "completed" {
		t.Fatalf("loop visits: %+v %v", visits, err)
	}
	run, err = s.GetPipelineRun(ctx, "s", "r")
	if err != nil || run.Status != "succeeded" {
		t.Fatalf("finished run: %+v %v", run, err)
	}
}

func TestPipelineRunOperations(t *testing.T) {
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
		if err := s.CreatePipelineRun(ctx, PipelineRun{ID: "bad", SwarmID: "s", OwnerID: "u", Graph: []byte(graph), Status: "running"}); err == nil {
			t.Fatal("invalid graph accepted")
		}
	}
	if err := s.CreatePipelineRun(ctx, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: []byte(runGraph), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPipelineRun(ctx, "other", "r"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong swarm: %v", err)
	}
	runs, err := s.ListPipelineRuns(ctx, "other")
	if err != nil || len(runs) != 0 {
		t.Fatalf("wrong swarm list: %v %v", runs, err)
	}
	r, err := s.GetPipelineRun(ctx, "s", "r")
	if err != nil || string(r.Graph) != runGraph || r.OwnerID != "u" {
		t.Fatalf("run: %+v %v", r, err)
	}
	const n = 12
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "ask", State: "waiting"})
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
		go func() { defer wg.Done(); results <- s.AnswerPipelineVisit(ctx, "s", "r", 1, "success") }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("answer retry: %v", err)
		}
	}
	visits, err := s.ListPipelineVisits(ctx, "s", "r")
	if err != nil || len(visits) != 2 || visits[0].State != "answered" || visits[1].NodeID != "actor" || visits[1].State != "pending" {
		t.Fatalf("concurrent progression: %+v %v", visits, err)
	}
	if _, err := s.AppendPipelineVisit(ctx, "other", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong swarm append: %v", err)
	}
	_, err = s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "actor", State: "inflight", ActorUID: "actor", ActorIncarnation: 2, RequestID: "req"})
	if err != nil {
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
	visits, err = s.ListPipelineVisits(ctx, "s", "r")
	if err != nil || len(visits) != 2 || visits[len(visits)-1].State != "unconfirmed" || visits[len(visits)-1].RequestID != "req" {
		t.Fatalf("recovery: %+v %v", visits, err)
	}
}

// BUG: Terminal runs accept visits and answers.
// HYPOTHESIS: Visit operations never inspect the run status.
// SOLUTION: Check active status within the write transaction.
// RESULT: FIXED — terminal runs reject both mutations.
// STATUS: FIXED
func TestPipelineTerminalRunRejectsVisits(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now')`)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"succeeded", "failed"} {
		t.Run(status, func(t *testing.T) {
			if err := s.CreatePipelineRun(ctx, PipelineRun{ID: status, SwarmID: "s", OwnerID: "u", Graph: []byte(runGraph), Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AppendPipelineVisit(ctx, "s", status, PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE pipeline_runs SET status=? WHERE id=?`, status, status); err != nil {
				t.Fatal(err)
			}
			if err := s.AnswerPipelineVisit(ctx, "s", status, 1, "success"); err == nil {
				t.Error("terminal run accepted answer")
			}
			if _, err := s.AppendPipelineVisit(ctx, "s", status, PipelineVisit{NodeID: "ask", State: "waiting"}); err == nil {
				t.Error("terminal run accepted visit")
			}
		})
	}
}

// BUG: A replacement run can be modified using an earlier run's graph.
// HYPOTHESIS: The graph read and write are separate SQLite transactions.
// SOLUTION: Hold an immediate transaction across validation and mutation.
// RESULT: FIXED — replacement cannot interleave between validation and mutation.
// STATUS: FIXED
type pipelineInterleaveContext struct {
	context.Context
	calls           atomic.Int32
	reached, resume chan struct{}
}

func (c *pipelineInterleaveContext) Value(key any) any {
	if _, ok := key.(txKey); ok && c.calls.Add(1) == 2 {
		close(c.reached)
		<-c.resume
	}
	return c.Context.Value(key)
}
func TestPipelineReplacementCannotInterleave(t *testing.T) {
	for _, operation := range []string{"append", "answer"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			other, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			_, err = s.db.Exec(`INSERT INTO users VALUES ('u','user',NULL,'hash',0,0,'now','now'); INSERT INTO projects VALUES ('p','project','now'); INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('s','p','u','swarm','space','now','now')`)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.CreatePipelineRun(ctx, PipelineRun{ID: "r", SwarmID: "s", OwnerID: "u", Graph: []byte(runGraph), Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if operation == "answer" {
				if _, err = s.AppendPipelineVisit(ctx, "s", "r", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
					t.Fatal(err)
				}
			}
			interleave := &pipelineInterleaveContext{Context: ctx, reached: make(chan struct{}), resume: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				if operation == "append" {
					_, e := s.AppendPipelineVisit(interleave, "s", "r", PipelineVisit{NodeID: "ask", State: "waiting"})
					done <- e
				} else {
					done <- s.AnswerPipelineVisit(interleave, "s", "r", 1, "success")
				}
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-interleave.reached:
				replacement := strings.Replace(runGraph, "outcome: success", "outcome: failure", 1)
				_, err = other.db.Exec(`DELETE FROM pipeline_runs WHERE id='r'; INSERT INTO pipeline_runs (id,swarm_id,owner_id,graph,status) VALUES ('r','s','u',?,'running'); INSERT INTO pipeline_steps (run_id,sequence,node_id,state) VALUES ('r',1,'ask','waiting')`, []byte(replacement))
				close(interleave.resume)
				if err != nil {
					t.Fatal(err)
				}
				err = <-done
				if err == nil {
					t.Fatal("operation modified replacement using old graph")
				}
			}
		})
	}
}
