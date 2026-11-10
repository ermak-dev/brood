package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Serve persisted pipeline runs; human gates are answered through
// the swarm request API (#216), the run has no answer route (#217).
// SPEC: Swarm authorization and CSRF apply; an answer moves the graph once.
// IMPLEMENTATION: Read scoped runs; answers go through AnswerSwarmRequest.
// RESULT: IMPLEMENTED — reads and gate answers are available without an execution engine.
// STATUS: DONE
const pipelineAPIGraph = `apiVersion: brood.dev/v1alpha1
name: test
start: ask
exit: done
nodes:
  ask:
    type: human
    question: Continue?
    options:
      - {label: yes, outcome: success}
  done: {type: exit}
edges:
  - {from: ask, to: done}
goalGates: [done]
`

// BUG: Run graph and chronological visit history are missing from the API.
// HYPOTHESIS: The detail view enumerates graph nodes instead of visits and no graph route is mounted.
// SOLUTION: Return ordered visits and render a scoped graph with escaped SVG text.
// RESULT: FIXED — repeated visits and graph are available on the wire.
// STATUS: FIXED
// BUG: Completed actor visits expose storage states instead of outcomes.
// HYPOTHESIS: The API maps answers but ignores completed actor outcomes.
// SOLUTION: Map persisted outcomes to display states and return the outcome.
// RESULT: FIXED — actor results appear in chronological visits.
// STATUS: FIXED
func TestPipelineWireContract(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, bob, swarm := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	graph := strings.Replace(pipelineAPIGraph, "start: ask", "start: entry", 1)
	graph = strings.Replace(graph, "nodes:\n", "nodes:\n  entry: {type: actor, role: development, prompt: work}\n", 1)
	graph = strings.Replace(graph, "  - {from: ask, to: done}", "  - {from: entry, to: ask}\n  - {from: ask, to: ask, condition: \"outcome=success\"}\n  - {from: ask, to: done, condition: \"outcome=fail\"}", 1)
	if err := insertSwarmGraph(ctx, ts.testServer, swarm, []byte(graph), "running"); err != nil {
		t.Fatal(err)
	}
	// Шаг актора кладётся напрямую: тест провода читает шаги, а привязку
	// роли к актору проверяет хранилище (#208).
	entry := store.PipelineVisit{Sequence: 1, NodeID: "entry", State: "inflight", ActorUID: "worker", ActorIncarnation: 1, RequestID: "request"}
	if err := insertSwarmStep(ctx, ts.testServer, swarm, entry); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.CompletePipelineActorVisit(ctx, swarm, swarm, entry.Sequence, "request", "worker", 1, "success"); err != nil {
		t.Fatal(err)
	}
	visits, err := ts.srv.store.SwarmSteps(ctx, swarm)
	if err != nil || len(visits) != 2 || visits[1].NodeID != "ask" || visits[1].State != "waiting" {
		t.Fatalf("first human visit: %+v %v", visits, err)
	}
	sequence := visits[1].Sequence
	answer := nodeAnswerPath(t, ts.srv, swarm, sequence)
	r0, b0, _ := ts.do(alice, "POST", answer, ts.csrf(alice), map[string]any{"outcome": "success"})
	expectStatus(t, "first answer", r0, b0, 200)
	visits, err = ts.srv.store.SwarmSteps(ctx, swarm)
	if err != nil || len(visits) != 3 {
		t.Fatalf("visits: %+v %v", visits, err)
	}
	if visits[1].Sequence != sequence || visits[1].NodeID != "ask" || visits[1].State != "answered" || visits[1].Answer == nil || *visits[1].Answer != "success" || visits[2].Sequence != sequence+1 || visits[2].NodeID != "ask" || visits[2].State != "waiting" {
		t.Fatalf("answer and successor: %+v", visits)
	}
	path := "/api/pipeline-runs/" + swarm
	r, b, _ := ts.do(alice, "POST", answer, ts.csrf(alice), map[string]any{"outcome": "success"})
	expectStatus(t, "retry before repeated gate", r, b, 200)
	visits, err = ts.srv.store.SwarmSteps(ctx, swarm)
	if err != nil || len(visits) != 3 || visits[2].State != "waiting" {
		t.Fatalf("retry must not answer repeated gate: %+v %v", visits, err)
	}
	r, b, _ = ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "detail", r, b, 200)
	if started, exists := b["startedAt"]; !exists || started != nil {
		t.Fatalf("unrecorded start must be null: %v", b)
	}
	nodes, ok := b["nodes"].([]any)
	if !ok || len(nodes) != 3 || nodes[0].(map[string]any)["id"] != "entry" || nodes[1].(map[string]any)["id"] != "ask" || nodes[2].(map[string]any)["id"] != "ask" {
		t.Fatalf("visits must preserve repetition: %v", b["nodes"])
	}
	if nodes[0].(map[string]any)["status"] != "done" || nodes[0].(map[string]any)["outcome"] != "success" || nodes[1].(map[string]any)["status"] != "done" || nodes[2].(map[string]any)["status"] != "active" {
		t.Fatalf("visits must preserve order and state: %v", nodes)
	}
	r, b, _ = ts.do(alice, "GET", path+"/graph", "", nil)
	expectStatus(t, "graph", r, b, 200)
	if svg, ok := b["svg"].(string); !ok || svg == "" {
		t.Fatalf("graph: %v", b)
	}
	r, b, _ = ts.do(bob, "GET", path+"/graph", "", nil)
	expectStatus(t, "hidden graph", r, b, 404)
	r, b, _ = ts.do(bob, "POST", nodeAnswerPath(t, ts.srv, swarm, sequence+1), ts.csrf(bob), map[string]any{"outcome": "success"})
	expectStatus(t, "outsider answer", r, b, 404)
}

// BUG: Internal answer failures were reported as stale conflicts.
// HYPOTHESIS: The handler classified every unrecognized store error as a conflict.
// SOLUTION: Match a store sentinel and forward other failures to the API error handler.
// RESULT: FIXED — internal failures return 500, not 409.
// STATUS: FIXED
func TestPipelineAnswerErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{store.ErrPipelineVisitConflict, 409},
		{store.ErrNotFound, 404},
		{errors.New("database unavailable"), 500},
	} {
		w := httptest.NewRecorder()
		swarmRequestError(w, httptest.NewRequest("POST", "/", nil), tc.err)
		if w.Code != tc.status {
			t.Errorf("%v: got %d, want %d", tc.err, w.Code, tc.status)
		}
	}
}

// BUG: Failed and unconfirmed actor visits were not represented faithfully.
// HYPOTHESIS: The API exposes raw storage state without the actor outcome.
// SOLUTION: Map confirmed outcomes and preserve the unconfirmed state.
// RESULT: FIXED — confirmed failure and uncertainty remain distinct.
// STATUS: FIXED
func TestPipelineActorStates(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, _, swarm := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	graph := strings.Replace(pipelineAPIGraph, "start: ask", "start: entry", 1)
	graph = strings.Replace(graph, "nodes:\n", "nodes:\n  entry: {type: actor, role: development, prompt: work}\n", 1)
	graph = strings.Replace(graph, "  - {from: ask, to: done}", "  - {from: entry, to: ask}\n  - {from: ask, to: done}", 1)
	// Каждый случай — отдельный рой проекта: один рой, один граф (#206).
	sw, err := ts.srv.store.GetSwarm(ctx, swarm)
	if err != nil {
		t.Fatal(err)
	}
	target := map[string]string{"failure": swarm}
	r, b, _ := ts.do(alice, "POST", "/api/projects/"+sw.ProjectID+"/swarms", ts.csrf(alice), map[string]string{"name": "uncertain"})
	expectStatus(t, "uncertain swarm", r, b, http.StatusCreated)
	target["uncertain"], _ = b["id"].(string)
	for _, tc := range []struct{ id, want string }{{"failure", "failed"}, {"uncertain", "unconfirmed"}} {
		if err := insertSwarmGraph(ctx, ts.testServer, target[tc.id], []byte(graph), "running"); err != nil {
			t.Fatal(err)
		}
		v := store.PipelineVisit{Sequence: 1, NodeID: "entry", State: "inflight", ActorUID: "worker", ActorIncarnation: 1, RequestID: "request"}
		if err := insertSwarmStep(ctx, ts.testServer, target[tc.id], v); err != nil {
			t.Fatal(err)
		}
		if tc.id == "failure" {
			err = ts.srv.store.CompletePipelineActorVisit(ctx, target[tc.id], target[tc.id], v.Sequence, "request", "worker", 1, "fail")
		} else {
			err = ts.srv.store.RecoverPipelineActions(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		r, b, _ := ts.do(alice, "GET", "/api/pipeline-runs/"+target[tc.id], "", nil)
		expectStatus(t, tc.id, r, b, 200)
		nodes := b["nodes"].([]any)
		node := nodes[0].(map[string]any)
		if node["status"] != tc.want || (tc.id == "failure" && node["outcome"] != "fail") || (tc.id == "uncertain" && node["outcome"] != nil) {
			t.Fatalf("%s: %v", tc.id, node)
		}
	}
}

// BUG: A completed exit is not displayed as done.
// HYPOTHESIS: Completed visits without an outcome are not mapped by node kind.
// SOLUTION: Map the completed designated exit without an outcome to done.
// RESULT: FIXED — the exit displays as done.
// STATUS: FIXED
func TestPipelineCompletedExit(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, _, swarm := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	if err := insertSwarmGraph(ctx, ts.testServer, swarm, []byte(pipelineAPIGraph), "running"); err != nil {
		t.Fatal(err)
	}
	v, err := ts.srv.store.AppendPipelineVisit(ctx, swarm, swarm, store.PipelineVisit{NodeID: "ask", State: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	r0, b0, _ := ts.do(alice, "POST", nodeAnswerPath(t, ts.srv, swarm, v.Sequence), ts.csrf(alice), map[string]any{"outcome": "success"})
	expectStatus(t, "answer", r0, b0, 200)
	r, b, _ := ts.do(alice, "GET", "/api/pipeline-runs/"+swarm, "", nil)
	expectStatus(t, "detail", r, b, 200)
	nodes := b["nodes"].([]any)
	node := nodes[len(nodes)-1].(map[string]any)
	if node["id"] != "done" || node["status"] != "done" {
		t.Fatalf("completed exit: %v", node)
	}
}

// BUG: A committed answer retry is rejected after its response is lost.
// HYPOTHESIS: The handler requires the visit to remain the latest waiting visit.
// SOLUTION: Allow an exact committed answer through to the store after matching its node.
// RESULT: FIXED — retries succeed without bypassing authorization or accepting conflicts.
// STATUS: FIXED
func TestPipelineHTTP(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, bob, swarm := actorsSetup(t, ts.testServer)
	err := insertSwarmGraph(context.Background(), ts.testServer, swarm, []byte(pipelineAPIGraph), "waiting")
	if err != nil {
		t.Fatal(err)
	}
	v, err := ts.srv.store.AppendPipelineVisit(context.Background(), swarm, swarm, store.PipelineVisit{NodeID: "ask", State: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	list := "/api/swarms/" + swarm + "/pipeline-runs"
	r, b, raw := ts.do(alice, "GET", list, "", nil)
	expectStatus(t, "list", r, b, 200)
	var runs []map[string]any
	if json.Unmarshal([]byte(raw), &runs) != nil || len(runs) != 1 {
		t.Fatalf("list: %s", raw)
	}
	r, b, _ = ts.do(bob, "GET", list, "", nil)
	expectStatus(t, "hidden list", r, b, 404)
	path := "/api/pipeline-runs/" + swarm
	r, b, _ = ts.do(bob, "GET", path, "", nil)
	expectStatus(t, "hidden run", r, b, 404)
	r, b, _ = ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "detail", r, b, 200)
	gate, ok := b["gate"].(map[string]any)
	if !ok || gate["visitSequence"] != float64(v.Sequence) {
		t.Fatalf("gate: %v", b)
	}
	r, b, _ = ts.do(alice, "POST", "/api/swarms/"+swarm+"/members", ts.csrf(alice), map[string]string{"username": "bob", "role": "viewer"})
	expectStatus(t, "add viewer", r, b, 204)
	r, b, _ = ts.do(bob, "GET", path, "", nil)
	expectStatus(t, "viewer detail", r, b, 200)
	if _, ok := b["gate"]; ok {
		t.Fatalf("viewer must not be offered an answer form: %v", b)
	}
	answer := nodeAnswerPath(t, ts.srv, swarm, v.Sequence)
	payload := map[string]any{"outcome": "success"}
	r, b, _ = ts.do(bob, "POST", answer, ts.csrf(bob), payload)
	expectStatus(t, "viewer cannot answer", r, b, 404)
	r, b, _ = ts.do(alice, "POST", answer, "", payload)
	expectStatus(t, "csrf", r, b, 403)
	r, b, _ = ts.do(alice, "POST", answer, ts.csrf(alice), payload)
	expectStatus(t, "answer", r, b, 200)
	r, b, _ = ts.do(alice, "GET", path, "", nil)
	expectStatus(t, "detail after answer", r, b, 200)
	stored, err := ts.srv.store.GetSwarm(context.Background(), swarm)
	if err != nil || b["status"] != legacyRunStatus(stored.Execution) {
		t.Fatalf("run status after answer %v, persisted execution %q: %v", b["status"], stored.Execution, err)
	}
	r, b, _ = ts.do(bob, "POST", answer, ts.csrf(bob), payload)
	expectStatus(t, "viewer cannot replay", r, b, 404)
	r, b, _ = ts.do(alice, "POST", answer, ts.csrf(alice), map[string]any{"outcome": "fail"})
	expectStatus(t, "conflicting answer", r, b, 409)
	r, b, _ = ts.do(alice, "POST", answer, ts.csrf(alice), payload)
	expectStatus(t, "lost response retry", r, b, 200)
	visits, err := ts.srv.store.SwarmSteps(context.Background(), swarm)
	if err != nil || len(visits) != 2 {
		t.Fatalf("retry must not create another visit: %+v %v", visits, err)
	}
}

// FEATURE: The run has no answer route of its own (#217).
// SPEC: The panel answers a gate through the swarm request API (#216), and
// neither bro nor MCP used the run route, so the old
// POST /api/pipeline-runs/{run}/nodes/{node}/answer is gone: it answers 404
// and leaves the gate open, while the run and its graph stay readable.
// IMPLEMENTATION: The route and its handler are removed from mountPipelineAPI.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestRunAnswerRouteIsGone(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, _, swarm := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	if err := insertSwarmGraph(ctx, ts.testServer, swarm, []byte(pipelineAPIGraph), "waiting"); err != nil {
		t.Fatal(err)
	}
	v, err := ts.srv.store.AppendPipelineVisit(ctx, swarm, swarm, store.PipelineVisit{NodeID: "ask", State: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/pipeline-runs/" + swarm
	r, b, _ := ts.do(alice, "POST", path+"/nodes/ask/answer", ts.csrf(alice), map[string]any{"answer": "success", "visitSequence": v.Sequence})
	expectStatus(t, "run answer route", r, b, http.StatusNotFound)
	requests, err := ts.srv.store.SwarmRequests(ctx, swarm)
	if err != nil || len(requests) != 1 || requests[0].State != "open" {
		t.Fatalf("the gate must stay open: %+v %v", requests, err)
	}
	steps, err := ts.srv.store.SwarmSteps(ctx, swarm)
	if err != nil || len(steps) != 1 || steps[0].State != "waiting" {
		t.Fatalf("the graph must not move: %+v %v", steps, err)
	}
	for _, read := range []string{path, path + "/graph", "/api/swarms/" + swarm + "/pipeline-runs"} {
		r, b, _ = ts.do(alice, "GET", read, "", nil)
		expectStatus(t, read, r, b, http.StatusOK)
	}
	r, b, _ = ts.do(alice, "POST", nodeAnswerPath(t, ts.srv, swarm, v.Sequence), ts.csrf(alice), map[string]any{"outcome": "success"})
	expectStatus(t, "swarm request answer", r, b, http.StatusOK)
}

// FEATURE: Human answer carries free text beside the route outcome (#209).
// SPEC: the response text is stored with the request and never picks the edge;
// only the answering user may replay the same answer; another manager gets 409.
// IMPLEMENTATION: swarmRequestAnswer calls AnswerSwarmRequest with the session check.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestPipelineAnswerStoresResponseAndRejectsForeignReplay(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, bob, swarm := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	if err := insertSwarmGraph(ctx, ts.testServer, swarm, []byte(pipelineAPIGraph), "waiting"); err != nil {
		t.Fatal(err)
	}
	v, err := ts.srv.store.AppendPipelineVisit(ctx, swarm, swarm, store.PipelineVisit{NodeID: "ask", State: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/members", ts.csrf(alice), map[string]string{"username": "bob", "role": "editor"})
	expectStatus(t, "add editor", r, b, 204)
	answer := nodeAnswerPath(t, ts.srv, swarm, v.Sequence)
	payload := map[string]any{"outcome": "success", "response": "fail, reject, done"}
	r, b, _ = ts.do(alice, "POST", answer, ts.csrf(alice), payload)
	expectStatus(t, "answer", r, b, 200)
	requests, err := ts.srv.store.SwarmRequests(ctx, swarm)
	if err != nil || len(requests) != 1 || requests[0].Outcome == nil || *requests[0].Outcome != "success" || requests[0].Response == nil || *requests[0].Response != "fail, reject, done" || requests[0].AnsweredBy == "" {
		t.Fatalf("stored request: %+v %v", requests, err)
	}
	r, b, _ = ts.do(alice, "POST", answer, ts.csrf(alice), payload)
	expectStatus(t, "own replay", r, b, 200)
	r, b, _ = ts.do(bob, "POST", answer, ts.csrf(bob), payload)
	expectStatus(t, "foreign replay", r, b, 409)
}

// FEATURE: Replay of a lost answer is accepted only from its author (#209).
// SPEC: same user, same outcome and same response, with a live session and
// current rights, returns 200 without writing; changed text, another user or
// lost rights return 409/4xx; a cancelled gate is not offered as active.
// This deviates from the issue text, which allowed any manager to replay.
// IMPLEMENTATION: AnswerSwarmRequest decides the replay inside its transaction.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestPipelineAnswerReplayAuthorAndCancelledGate(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	admin, alice, bob, swarm := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	if err := insertSwarmGraph(ctx, ts.testServer, swarm, []byte(pipelineAPIGraph), "waiting"); err != nil {
		t.Fatal(err)
	}
	v, err := ts.srv.store.AppendPipelineVisit(ctx, swarm, swarm, store.PipelineVisit{NodeID: "ask", State: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/members", ts.csrf(alice), map[string]string{"username": "bob", "role": "editor"})
	expectStatus(t, "add editor", r, b, 204)
	answer := nodeAnswerPath(t, ts.srv, swarm, v.Sequence)
	payload := func(text string) map[string]any {
		return map[string]any{"outcome": "success", "response": text}
	}
	stored := func() store.SwarmRequest {
		all, err := ts.srv.store.SwarmRequests(ctx, swarm)
		if err != nil || len(all) == 0 {
			t.Fatalf("requests: %v %v", all, err)
		}
		return all[0]
	}
	r, b, _ = ts.do(bob, "POST", answer, ts.csrf(bob), payload("approved"))
	expectStatus(t, "answer", r, b, 200)
	first := stored()
	r, b, _ = ts.do(bob, "POST", answer, ts.csrf(bob), payload("approved"))
	expectStatus(t, "author replay", r, b, 200)
	r, b, _ = ts.do(bob, "POST", answer, ts.csrf(bob), payload("changed"))
	expectStatus(t, "changed text", r, b, 409)
	r, b, _ = ts.do(alice, "POST", answer, ts.csrf(alice), payload("approved"))
	expectStatus(t, "other manager", r, b, 409)
	if after := stored(); !reflect.DeepEqual(first, after) {
		t.Fatalf("replays rewrote the answer: %+v", after)
	}
	// Автор лишён роли: повтор не проходит.
	r, b, _ = ts.do(alice, "DELETE", "/api/swarms/"+swarm+"/members/bob", ts.csrf(alice), nil)
	if r.StatusCode >= 300 {
		t.Fatalf("remove member: %d %v", r.StatusCode, b)
	}
	r, b, _ = ts.do(bob, "POST", answer, ts.csrf(bob), payload("approved"))
	if r.StatusCode == 200 {
		t.Fatalf("revoked author replay accepted: %v", b)
	}
	// Выход без сессии тоже не повторяет.
	r, b, _ = ts.do(newClient(t), "POST", answer, "", payload("approved"))
	if r.StatusCode == 200 {
		t.Fatalf("anonymous replay accepted: %v", b)
	}
	_ = admin
}

func TestPipelineCancelledGateIsNotOffered(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, _, swarm := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	if err := insertSwarmGraph(ctx, ts.testServer, swarm, []byte(pipelineAPIGraph), "waiting"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.srv.store.AppendPipelineVisit(ctx, swarm, swarm, store.PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(alice, "GET", "/api/pipeline-runs/"+swarm, "", nil)
	expectStatus(t, "detail", r, b, 200)
	if _, ok := b["gate"]; !ok {
		t.Fatalf("waiting gate is missing: %v", b)
	}
	if err := ts.srv.store.SetSwarmExecution(ctx, swarm, "stopped"); err != nil {
		t.Fatal(err)
	}
	r, b, _ = ts.do(alice, "GET", "/api/pipeline-runs/"+swarm, "", nil)
	expectStatus(t, "detail after stop", r, b, 200)
	if _, ok := b["gate"]; ok {
		t.Fatalf("cancelled gate offered: %v", b)
	}
	if err := ts.srv.store.SetSwarmExecution(ctx, swarm, "running"); err == nil {
		t.Fatal("stopped graph resumed on a cancelled gate")
	}
	r, b, _ = ts.do(alice, "GET", "/api/pipeline-runs/"+swarm, "", nil)
	if _, ok := b["gate"]; ok {
		t.Fatalf("cancelled gate offered after refused resume: %v", b)
	}
}

// FEATURE: The swarm page shows when each step ran and what the graph asks
// of the selected node (#215).
// SPEC: Every step carries its sequence and its recorded start and finish;
// the graph lists each node with its kind and the instruction the graph
// gives it: the actor prompt or the human question.
// IMPLEMENTATION: Copy the stored step moments and the parsed node fields
// into the existing read views.
// RESULT: IMPLEMENTED — the timing and the instruction are on the wire.
// STATUS: DONE
func TestPipelineStepTimesAndNodeInstructions(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	_, alice, _, swarm := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	graph := strings.Replace(pipelineAPIGraph, "start: ask", "start: entry", 1)
	graph = strings.Replace(graph, "nodes:\n", "nodes:\n  entry: {type: actor, role: development, prompt: Write the plan}\n", 1)
	graph = strings.Replace(graph, "  - {from: ask, to: done}", "  - {from: entry, to: ask}\n  - {from: ask, to: done}", 1)
	if err := insertSwarmGraph(ctx, ts.testServer, swarm, []byte(graph), "running"); err != nil {
		t.Fatal(err)
	}
	// Шаг актора кладётся напрямую, как в TestPipelineWireContract: привязку
	// роли к актору проверяет хранилище (#208), здесь читается провод.
	entry := store.PipelineVisit{Sequence: 1, NodeID: "entry", State: "inflight", ActorUID: "worker", ActorIncarnation: 1, RequestID: "request"}
	if err := insertSwarmStep(ctx, ts.testServer, swarm, entry); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.CompletePipelineActorVisit(ctx, swarm, swarm, entry.Sequence, "request", "worker", 1, "success"); err != nil {
		t.Fatal(err)
	}
	steps, err := ts.srv.store.SwarmSteps(ctx, swarm)
	if err != nil || len(steps) != 2 || steps[0].StartedAt == nil || steps[0].FinishedAt == nil || steps[1].FinishedAt != nil {
		t.Fatalf("stored steps: %+v %v", steps, err)
	}
	r, b, _ := ts.do(alice, "GET", "/api/pipeline-runs/"+swarm, "", nil)
	expectStatus(t, "detail", r, b, 200)
	nodes, ok := b["nodes"].([]any)
	if !ok || len(nodes) != 2 {
		t.Fatalf("steps: %v", b["nodes"])
	}
	first, second := nodes[0].(map[string]any), nodes[1].(map[string]any)
	if first["sequence"] != float64(steps[0].Sequence) || first["startedAt"] != *steps[0].StartedAt || first["finishedAt"] != *steps[0].FinishedAt {
		t.Fatalf("finished step must carry its sequence and moments: %v", first)
	}
	if _, finished := second["finishedAt"]; finished || second["sequence"] != float64(steps[1].Sequence) {
		t.Fatalf("waiting step has no finish: %v", second)
	}
	if steps[1].StartedAt == nil {
		if _, started := second["startedAt"]; started {
			t.Fatalf("unrecorded start must stay absent: %v", second)
		}
	} else if second["startedAt"] != *steps[1].StartedAt {
		t.Fatalf("waiting step start: %v", second)
	}
	r, b, _ = ts.do(alice, "GET", "/api/pipeline-runs/"+swarm+"/graph", "", nil)
	expectStatus(t, "graph", r, b, 200)
	listed, ok := b["nodes"].([]any)
	if !ok || len(listed) != 3 {
		t.Fatalf("graph nodes: %v", b["nodes"])
	}
	want := []map[string]any{
		{"id": "ask", "kind": "human", "question": "Continue?"},
		{"id": "done", "kind": "exit"},
		{"id": "entry", "kind": "actor", "prompt": "Write the plan"},
	}
	for i, node := range listed {
		got := node.(map[string]any)
		if len(got) != len(want[i]) {
			t.Fatalf("graph node %d: got %v, want %v", i, got, want[i])
		}
		for key, value := range want[i] {
			if got[key] != value {
				t.Fatalf("graph node %d: got %v, want %v", i, got, want[i])
			}
		}
	}
}
