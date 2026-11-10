package server

// FEATURE: HTTP запуска и остановки роя (#211).
// SPEC: POST /api/swarms/{id}/start {definition, commit} и
// POST /api/swarms/{id}/stop. Видящему рой без права управлять — 403,
// постороннему рой скрыт — 404. Отказы называют причину и ничего не пишут;
// запуск и остановка не создают, не останавливают и не удаляют акторов.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/store"
)

const (
	startAPIOrigin = "https://git.example.org/team/repo.git"
	startAPICommit = "89abcdef0123456789abcdef0123456789abcdef"
)

const startAPIRelease = `apiVersion: brood.dev/v1alpha1
name: release
start: ask
exit: done
nodes:
  ask:
    type: human
    question: Ship?
    options:
      - {label: Ship, outcome: success}
      - {label: Hold, outcome: fail}
  build: {type: actor, role: build, prompt: work}
  done: {type: exit}
edges:
  - {from: ask, to: build, label: Ship}
  - {from: ask, to: done, label: Hold}
  - {from: build, to: done}
goalGates: [done]
`

const startAPIApprove = `apiVersion: brood.dev/v1alpha1
name: approve
start: ask
exit: done
nodes:
  ask:
    type: human
    question: Approve?
    options:
      - {label: Approve, outcome: success}
  done: {type: exit}
edges:
  - {from: ask, to: done}
goalGates: [done]
`

// startAPIReview — роль review в рое не привязана.
const startAPIReview = `apiVersion: brood.dev/v1alpha1
name: review
start: check
exit: done
nodes:
  check: {type: actor, role: review, prompt: review}
  done: {type: exit}
edges:
  - {from: check, to: done}
goalGates: [done]
`

type startAPIFixture struct {
	ts      *substrateServer
	alice   *http.Client
	bob     *http.Client
	pve     *managedFakePVE
	master  store.ManagedActor
	swarm   string
	project string
}

// startAPISetup — рой alice с работающим актором master, привязанным к роли
// build; у проекта есть repository и каталог: release, approve, review и
// неисполнимый retrying. bob — участник проекта вне роя. У соседнего
// проекта своё определение foreign.
func startAPISetup(t *testing.T) startAPIFixture {
	t.Helper()
	ts, alice, master, pve := masterSetup(t)
	ctx := context.Background()
	s := ts.srv.store
	sw, err := s.GetSwarm(ctx, master.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectRepository(ctx, sw.ProjectID, startAPIOrigin); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSwarmRole(ctx, store.SwarmRoleBinding{SwarmID: sw.ID, Role: "build", ActorUID: master.UID}); err != nil {
		t.Fatal(err)
	}
	root, err := s.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	allowAll := func(context.Context) bool { return true }
	if _, err := s.SyncWorkflowDefinitions(ctx, sw.ProjectID, root.ID, map[string]string{
		"release":  startAPIRelease,
		"approve":  startAPIApprove,
		"review":   startAPIReview,
		"retrying": strings.Replace(startAPIApprove, "name: approve", "name: retrying", 1) + "retry: {maxRetries: 3}\n",
	}, allowAll); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, store.Project{ID: "neighbour", Name: "neighbour"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncWorkflowDefinitions(ctx, "neighbour", root.ID, map[string]string{"foreign": strings.Replace(startAPIApprove, "name: approve", "name: foreign", 1)}, allowAll); err != nil {
		t.Fatal(err)
	}
	bob := newClient(t)
	ts.login(bob, "bob", "bob-password-1")
	return startAPIFixture{ts: ts, alice: alice, bob: bob, pve: pve, master: master, swarm: sw.ID, project: sw.ProjectID}
}

func (f startAPIFixture) start(client *http.Client, body any) (*http.Response, map[string]any) {
	r, b, _ := f.ts.do(client, "POST", "/api/swarms/"+f.swarm+"/start", f.ts.csrf(client), body)
	return r, b
}

func (f startAPIFixture) stop(client *http.Client) (*http.Response, map[string]any) {
	r, b, _ := f.ts.do(client, "POST", "/api/swarms/"+f.swarm+"/stop", f.ts.csrf(client), nil)
	return r, b
}

// lifeState — акторы роя, их привязки и обращения к Proxmox: запуск и
// остановка не меняют ничего из этого.
func (f startAPIFixture) lifeState(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	actors, err := f.ts.srv.store.ListManagedActors(ctx, f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := f.ts.srv.store.SwarmsRoleBindings(ctx, f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	f.pve.mu.Lock()
	defer f.pve.mu.Unlock()
	var out bytes.Buffer
	for _, a := range actors {
		fmt.Fprintf(&out, "%+v\n", a)
	}
	for _, b := range bindings {
		out.WriteString(b.Role + " " + b.ActorUID + "\n")
	}
	out.WriteString(strings.Repeat("|", f.pve.creates) + "c" + strings.Repeat("|", f.pve.starts) + "s" + strings.Repeat("|", f.pve.stops) + "x" + strings.Repeat("|", f.pve.deletes) + "d")
	return out.String()
}

// executionState — то, что запуск может записать.
func (f startAPIFixture) executionState(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	sw, err := f.ts.srv.store.GetSwarm(ctx, f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := f.ts.srv.store.SwarmSteps(ctx, f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := f.ts.srv.store.SwarmRequests(ctx, f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	out.WriteString(sw.State + " " + sw.Execution + " " + string(sw.Graph) + " " + sw.DefinitionName + "\n")
	for _, v := range steps {
		out.WriteString(v.NodeID + " " + v.State + "\n")
	}
	for _, q := range requests {
		out.WriteString(q.ID + " " + q.State + "\n")
	}
	return out.String()
}

func TestSwarmStartStopWire(t *testing.T) {
	f := startAPISetup(t)
	ctx := context.Background()
	life := f.lifeState(t)
	start := map[string]any{"definition": "release", "commit": startAPICommit}

	r, b := f.start(f.bob, start)
	expectStatus(t, "outsider start", r, b, http.StatusNotFound)
	r, b = f.stop(f.bob)
	expectStatus(t, "outsider stop", r, b, http.StatusNotFound)
	r, b, _ = f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/members", f.ts.csrf(f.alice), map[string]string{"username": "bob", "role": "viewer"})
	expectStatus(t, "add viewer", r, b, http.StatusNoContent)
	r, b = f.start(f.bob, start)
	expectStatus(t, "viewer start", r, b, http.StatusForbidden)

	r, b, _ = f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/start", "", start)
	expectStatus(t, "start without CSRF", r, b, http.StatusForbidden)
	r, b, _ = f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/stop", "", nil)
	expectStatus(t, "stop without CSRF", r, b, http.StatusForbidden)

	r, b = f.start(f.alice, start)
	expectStatus(t, "start", r, b, http.StatusOK)
	catalog, err := f.ts.srv.store.GetWorkflowDefinition(ctx, f.project, "release")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := b["definition"].(map[string]any)
	source, _ := b["source"].(map[string]any)
	step, _ := b["step"].(map[string]any)
	if b["swarmId"] != f.swarm || b["execution"] != "waiting" || definition["name"] != "release" || definition["version"] != float64(catalog.Version) || definition["revision"] != catalog.Revision {
		t.Fatalf("start response: %v", b)
	}
	if source["origin"] != startAPIOrigin || source["commit"] != startAPICommit || step["sequence"] != float64(1) || step["nodeId"] != "ask" || step["state"] != "waiting" {
		t.Fatalf("start response source or step: %v", b)
	}
	if got := f.lifeState(t); got != life {
		t.Fatalf("start touched actors:\n%s\nwant\n%s", got, life)
	}

	r, b = f.start(f.alice, start)
	expectStatus(t, "second start", r, b, http.StatusConflict)
	if !strings.Contains(b["detail"].(string), "execution is waiting") {
		t.Fatalf("second start reason: %v", b)
	}
	r, b = f.stop(f.bob)
	expectStatus(t, "viewer stop", r, b, http.StatusForbidden)
	r, b = f.stop(f.alice)
	expectStatus(t, "stop", r, b, http.StatusOK)
	if b["execution"] != "stopped" || b["step"].(map[string]any)["state"] != "stopped" {
		t.Fatalf("stop response: %v", b)
	}
	if got := f.lifeState(t); got != life {
		t.Fatalf("stop touched actors, workspace or bindings:\n%s\nwant\n%s", got, life)
	}
	r, b = f.stop(f.alice)
	expectStatus(t, "repeated stop", r, b, http.StatusConflict)
	if !strings.Contains(b["detail"].(string), "execution is stopped") {
		t.Fatalf("repeated stop reason: %v", b)
	}
	frozen := f.executionState(t)
	r, b, _ = f.ts.do(f.alice, "POST", "/api/pipeline-runs/"+f.swarm+"/nodes/ask/answer", f.ts.csrf(f.alice), map[string]any{"answer": "success", "visitSequence": 1})
	expectStatus(t, "answer after stop", r, b, http.StatusConflict)
	if got := f.executionState(t); got != frozen {
		t.Fatalf("answer after stop advanced the graph:\n%s\nwant\n%s", got, frozen)
	}

	r, b = f.start(f.alice, map[string]any{"definition": "release"})
	expectStatus(t, "restart", r, b, http.StatusOK)
	if b["step"].(map[string]any)["sequence"] != float64(2) || b["source"].(map[string]any)["commit"] != startAPICommit {
		t.Fatalf("restart response: %v", b)
	}
	r, b = f.start(f.alice, map[string]any{"definition": "approve"})
	expectStatus(t, "restart with another definition", r, b, http.StatusConflict)
	if got := f.lifeState(t); got != life {
		t.Fatalf("restart touched actors:\n%s\nwant\n%s", got, life)
	}
}

// TestSwarmStartRefusalsWire — отказы запуска до записи, каждый со своим
// статусом и причиной; неисполнимый граф проверяется через сам запуск.
func TestSwarmStartRefusalsWire(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   any
		setup  func(*testing.T, startAPIFixture)
		status int
		reason string
	}{
		{name: "definition missing", body: map[string]any{"definition": "absent", "commit": startAPICommit}, status: http.StatusNotFound, reason: `"absent"`},
		{name: "definition of another project", body: map[string]any{"definition": "foreign", "commit": startAPICommit}, status: http.StatusNotFound, reason: `"foreign"`},
		{name: "graph the runner refuses", body: map[string]any{"definition": "retrying", "commit": startAPICommit}, status: http.StatusUnprocessableEntity, reason: "retries"},
		{name: "unbound role", body: map[string]any{"definition": "review", "commit": startAPICommit}, status: http.StatusConflict, reason: `role "review"`},
		{name: "actor not running", body: map[string]any{"definition": "release", "commit": startAPICommit}, setup: func(t *testing.T, f startAPIFixture) {
			a, err := f.ts.srv.store.GetManagedActor(context.Background(), f.master.UID)
			if err != nil {
				t.Fatal(err)
			}
			a.Phase = "provisioning"
			if _, err := f.ts.srv.store.UpdateManagedActor(context.Background(), a, a.Revision); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusConflict, reason: "provisioning"},
		{name: "swarm not active", body: map[string]any{"definition": "release", "commit": startAPICommit}, setup: func(t *testing.T, f startAPIFixture) {
			if err := f.ts.srv.store.SetSwarmState(context.Background(), f.swarm, "error"); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusConflict, reason: "swarm is error"},
		{name: "commit missing", body: map[string]any{"definition": "release"}, status: http.StatusUnprocessableEntity, reason: "commit"},
		{name: "project without repository", body: map[string]any{"definition": "release", "commit": startAPICommit}, setup: func(t *testing.T, f startAPIFixture) {
			if err := f.ts.srv.store.SetProjectRepository(context.Background(), f.project, ""); err != nil {
				t.Fatal(err)
			}
		}, status: http.StatusUnprocessableEntity, reason: "repository"},
		{name: "definition absent from body", body: map[string]any{"commit": startAPICommit}, status: http.StatusBadRequest, reason: "definition"},
		{name: "unknown field", body: map[string]any{"definition": "release", "commit": startAPICommit, "graph": "x"}, status: http.StatusBadRequest, reason: "definition"},
		{name: "definition not a string", body: map[string]any{"definition": 7, "commit": startAPICommit}, status: http.StatusBadRequest, reason: "definition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startAPISetup(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			before := f.executionState(t)
			life := f.lifeState(t)
			r, b := f.start(f.alice, tc.body)
			expectStatus(t, tc.name, r, b, tc.status)
			if detail, _ := b["detail"].(string); !strings.Contains(detail, tc.reason) {
				t.Fatalf("%s: reason %q does not name %q", tc.name, detail, tc.reason)
			}
			if got := f.executionState(t); got != before {
				t.Fatalf("refused start wrote:\n%s\nwant\n%s", got, before)
			}
			if got := f.lifeState(t); got != life {
				t.Fatalf("refused start touched actors:\n%s\nwant\n%s", got, life)
			}
		})
	}
}

// TestSwarmStartDuplicateField — неоднозначное тело отвергается, а не
// решается последним ключом.
func TestSwarmStartDuplicateField(t *testing.T) {
	f := startAPISetup(t)
	req, err := http.NewRequest("POST", f.ts.http.URL+"/api/swarms/"+f.swarm+"/start", strings.NewReader(`{"definition":"approve","definition":"release","commit":"`+startAPICommit+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", f.ts.http.URL)
	req.Header.Set("X-CSRFToken", f.ts.csrf(f.alice))
	resp, err := f.alice.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate definition: status %d", resp.StatusCode)
	}
}

// TestSwarmStopRefusesFinished — остановка завершённого исполнения не
// возвращает молчаливый успех.
func TestSwarmStopRefusesFinished(t *testing.T) {
	f := startAPISetup(t)
	r, b := f.stop(f.alice)
	expectStatus(t, "stop idle", r, b, http.StatusConflict)
	if !strings.Contains(b["detail"].(string), "execution is idle") {
		t.Fatalf("stop idle reason: %v", b)
	}
	r, b = f.start(f.alice, map[string]any{"definition": "approve", "commit": startAPICommit})
	expectStatus(t, "start", r, b, http.StatusOK)
	r, b, _ = f.ts.do(f.alice, "POST", "/api/pipeline-runs/"+f.swarm+"/nodes/ask/answer", f.ts.csrf(f.alice), map[string]any{"answer": "success", "visitSequence": 1})
	expectStatus(t, "approve", r, b, http.StatusOK)
	before := f.executionState(t)
	r, b = f.stop(f.alice)
	expectStatus(t, "stop succeeded", r, b, http.StatusConflict)
	if !strings.Contains(b["detail"].(string), "execution is succeeded") {
		t.Fatalf("stop succeeded reason: %v", b)
	}
	if got := f.executionState(t); got != before {
		t.Fatalf("refused stop wrote:\n%s\nwant\n%s", got, before)
	}
}

// TestSwarmStartConcurrentWire — одновременные запуски одного роя через HTTP
// дают одно исполнение и один первый шаг.
func TestSwarmStartConcurrentWire(t *testing.T) {
	f := startAPISetup(t)
	token := f.ts.csrf(f.alice)
	const starters = 8
	statuses := make([]int, starters)
	var ready, done sync.WaitGroup
	ready.Add(starters)
	done.Add(starters)
	gate := make(chan struct{})
	for i := range starters {
		go func() {
			defer done.Done()
			req, err := http.NewRequest("POST", f.ts.http.URL+"/api/swarms/"+f.swarm+"/start", strings.NewReader(`{"definition":"release","commit":"`+startAPICommit+`"}`))
			if err != nil {
				t.Error(err)
				ready.Done()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", f.ts.http.URL)
			req.Header.Set("X-CSRFToken", token)
			ready.Done()
			<-gate
			resp, err := f.alice.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			statuses[i] = resp.StatusCode
		}()
	}
	ready.Wait()
	close(gate)
	done.Wait()
	started := 0
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			started++
		case http.StatusConflict:
		default:
			t.Fatalf("concurrent start statuses: %v", statuses)
		}
	}
	steps, err := f.ts.srv.store.SwarmSteps(context.Background(), f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	if started != 1 || len(steps) != 1 {
		t.Fatalf("concurrent starts: %d succeeded, steps %+v", started, steps)
	}
}
