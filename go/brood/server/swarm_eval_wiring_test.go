package server

// BUG: Восстановление теряет eval после первой ограниченной страницы.
// HYPOTHESIS: Startup hook читает только одну страницу LIMIT 256.
// SOLUTION: Стабильный курсор swarm ID; startup обходит все страницы с отменой.
// RESULT: FIXED — все 270 ключей посещены без изменения вопросов и выполнения.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/workflowpolicy"

	"brood.dev/go/brood/store"
)

func TestEvalStartupVisitsAllPagesWithoutMutation(t *testing.T) {
	f := evalAPISetup(t)
	f.ts.srv.steps.close()
	ctx := context.Background()
	s := f.ts.srv.store
	sw, err := s.GetSwarm(ctx, f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	auth := func(context.Context, string, store.SwarmEvalBinding) error { return nil }
	for i := range 270 {
		id := fmt.Sprintf("page-%03d", i)
		err = s.CreateSwarm(ctx, store.Swarm{ID: id, ProjectID: sw.ProjectID, OwnerID: f.launcher, Name: id, AtespaceName: id, State: "active"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.CreateSwarmEvalBinding(ctx, store.SwarmEvalBindingInput{CandidateSwarmID: id, ReferenceSwarmID: f.reference, ReferencePass: 1, Definition: "approve", Model: "selected-model-1", LauncherID: f.launcher}, auth)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.StartSwarmEval(ctx, id, f.launcher, auth); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Несколько чтений курсором обязаны вернуть и поздние ключи без мутаций.
	var visited []string
	for after := ""; ; {
		page, err := s.WaitingSwarmEvalSwarms(ctx, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 256 {
			t.Fatalf("unbounded page: %d", len(page))
		}
		if len(page) == 0 {
			break
		}
		for _, id := range page {
			if id <= after {
				t.Fatalf("cursor failed to advance: %q after %q", id, after)
			}
			after = id
			visited = append(visited, id)
		}
	}
	if len(visited) != len(ids) {
		t.Fatalf("paged read visited %d, want %d", len(visited), len(ids))
	}
	for i, id := range ids {
		if visited[i] != id {
			t.Fatalf("paged read key %d: %q, want %q", i, visited[i], id)
		}
	}
	d := newStepDispatcher(f.ts.srv)
	// Уже идущие проходы сливают kick в again, не меняя строки Store.
	for _, id := range ids {
		d.running[id] = true
	}
	d.start(ctx)
	for _, id := range ids {
		if !d.again[id] {
			t.Errorf("startup never visited eligible eval %s", id)
		}
		current, err := s.GetSwarm(ctx, id)
		if err != nil || current.Execution != "waiting" {
			t.Fatalf("scan mutated %s: %+v %v", id, current, err)
		}
		requests, err := s.SwarmRequests(ctx, id)
		if err != nil || len(requests) != 1 || requests[0].State != "open" || requests[0].AnsweredBy != "" {
			t.Fatalf("scan mutated question: %+v %v", requests, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.kickPending(canceled); err == nil {
		t.Fatal("canceled startup scan succeeded")
	}
}

func evalRawRequest(t *testing.T, f evalAPIFixture, suffix, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", f.ts.http.URL+"/api/swarms/"+f.candidate+suffix, strings.NewReader(body))
	req.Header.Set("Origin", f.ts.http.URL)
	req.Header.Set(auth.HeaderCSRF, f.ts.csrf(f.alice))
	req.Header.Set("Content-Type", "application/json")
	for _, cookie := range f.alice.Jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	return req
}

// BUG: Допустимые пробелы пустой eval selection приводят к отказу.
// HYPOTHESIS: Обработчик сравнивает байты вместо разбора пустого JSON.
// SOLUTION: Ограниченный strict-разбор object/null после удаления JSON-пробелов.
// RESULT: FIXED — пустые формы приняты, непустые/повреждённые отвергнуты.
// STATUS: FIXED
func TestEvalStartEmptySelectionJSON(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{"", 200}, {" \n\t", 200}, {"null", 200}, {" \n null \t", 200}, {"{}", 200}, {" { \n\t } \n", 200},
		{`{"model":"x"}`, 400}, {`{"x":null}`, 400}, {`{"x":1,"x":1}`, 400},
		{`{"Model":"x"}`, 400}, {`[]`, 400}, {`""`, 400}, {`{} null`, 400}, {`{`, 400}, {`nul`, 400},
		{" null", 400}, {" ", 400}, {"\xffnull", 400},
		{strings.Repeat(" ", maxSwarmStartBody+1), 400},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("body-%q", tc.body[:min(len(tc.body), 30)]), func(t *testing.T) {
			f := evalAPISetup(t)
			f.ts.srv.steps.close()
			f.post(t, "/eval", f.input, 200)
			w := httptest.NewRecorder()
			f.ts.srv.Handler().ServeHTTP(w, evalRawRequest(t, f, "/eval/start", tc.body))
			if w.Code != tc.want {
				t.Fatalf("body %q: status %d, want %d: %s", tc.body, w.Code, tc.want, w.Body)
			}
			if tc.want == 400 {
				sw, err := f.ts.srv.store.GetSwarm(context.Background(), f.candidate)
				if err != nil || sw.Execution != "idle" {
					t.Fatalf("invalid body started eval: %+v %v", sw, err)
				}
			}
		})
	}
}

// BUG: Отменённый bind ждёт занятый project gate до освобождения владельцем.
// HYPOTHESIS: HTTP-путь использует admission с фоновым контекстом.
// SOLUTION: AdmitWorkloadContext получает контекст HTTP-запроса.
// RESULT: FIXED — запрос завершён до освобождения gate, binding не записан.
// STATUS: FIXED
func TestEvalCanceledBindLeavesBusyGate(t *testing.T) {
	f := evalAPISetup(t)
	hold := ephemeralGateHold{entered: make(chan struct{}), release: make(chan struct{})}
	held := make(chan error, 1)
	go func() { held <- f.ts.srv.app.AdmitWorkload(f.launcher, f.candidate, hold) }()
	<-hold.entered
	defer func() { close(hold.release); <-held }()
	raw, err := json.Marshal(f.input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := evalRawRequest(t, f, "/eval", string(raw)).WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); f.ts.srv.Handler().ServeHTTP(httptest.NewRecorder(), req) }()
	select {
	case <-done:
		t.Fatal("bind escaped busy gate")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled bind still blocked behind busy project gate")
	}
	if _, err := f.ts.srv.store.GetSwarmEvalBinding(context.Background(), f.candidate); err != store.ErrNotFound {
		t.Fatalf("canceled bind persisted: %v", err)
	}
}

func evalActorDispatchFixture(t *testing.T, policy bool) (evalAPIFixture, *dispatchServer, store.ManagedActor) {
	t.Helper()
	f := evalAPISetup(t)
	// Синтетический актор существует только в Store, без контейнера у провайдера.
	f.ts.srv.managed.stopReconcile()
	ctx := context.Background()
	s := f.ts.srv.store
	sw, err := s.GetSwarm(ctx, f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(dispatchGraph, "name: deliver", "name: approve", 1)
	if policy {
		graph = strings.Replace(graph, "role: build,", "role: build, actionClasses: [landing],", 1)
	}
	if _, err = s.SyncWorkflowDefinitions(ctx, sw.ProjectID, root.ID, map[string]string{"approve": graph}, func(context.Context) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if policy {
		if _, err = s.SetProjectWorkflowPolicy(ctx, root.ID, sw.ProjectID, 0, []workflowpolicy.Rule{{ID: "land", Actions: []string{"landing"}, Require: workflowpolicy.Human}}, func(context.Context) bool { return true }); err != nil {
			t.Fatal(err)
		}
	}
	f.post(t, "/eval", f.input, 200)
	evalDispatchActor(t, f, "first", 101)
	a, err := s.GetManagedActor(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "stopped")
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "start-first", "start")
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, a.Stage, "running")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindSwarmRole(ctx, store.SwarmRoleBinding{SwarmID: f.candidate, Role: "build", ActorUID: a.UID}); err != nil {
		t.Fatal(err)
	}
	if err = s.BindSwarmRole(ctx, store.SwarmRoleBinding{SwarmID: f.candidate, Role: "review", ActorUID: a.UID}); err != nil {
		t.Fatal(err)
	}
	d := &dispatchServer{substrateServer: f.ts, t: t, swarm: f.candidate, worker: a}
	key := workspaceAttestationKey(strings.Repeat("k", 32))
	f.ts.srv.managed.registry = &proxmoxRegistry{relaySecret: strings.Repeat("k", 32)}
	d.reply = func(call guestAttest) (relay.SessionResponse, error) {
		return signedAttestation(t, key, call, call.Generation)
	}
	d.deliver = func(deliveredMessage) (relay.MessageResponse, error) {
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	f.ts.srv.managed.hub = commandSessionHub{guestRelay: &managedFakeRelay{connected: true}, call: d.callSession}
	f.ts.srv.managed.deliverMessage = d.deliverMessage
	return f, d, a
}

// BUG: Вопрос актора после завершения первого dispatch не запускает replay.
// HYPOTHESIS: ask_operator записывает вопрос без пробуждения диспетчера.
// SOLUTION: Успешный commit открытого eval-вопроса будит доверенный dispatcher.
// RESULT: FIXED — текущий гость останавливает eval без ответа и новых эффектов.
// STATUS: FIXED
func TestEvalGuestQuestionWakesIdleDispatcher(t *testing.T) {
	f, d, a := evalActorDispatchFixture(t, false)
	f.post(t, "/eval/start", nil, 200)
	f.ts.srv.steps.wait()
	if d.committed().State != "inflight" {
		t.Fatalf("initial dispatch not delivered: %+v", d.committed())
	}
	f.ts.srv.steps.mu.Lock()
	running := len(f.ts.srv.steps.running)
	f.ts.srv.steps.mu.Unlock()
	if running != 0 {
		t.Fatalf("original dispatcher not idle: %d", running)
	}
	inv := relay.SwarmRequest{Tool: "ask_operator", Arguments: json.RawMessage(`{"question":"Can I publish?","kind":"text"}`)}
	// Устаревшая инкарнация не может ни открыть вопрос, ни запустить replay.
	if got := f.ts.srv.managed.handleSwarmRequest(context.Background(), a.UID, a.Incarnation+1, inv); got.OK {
		t.Fatal("stale actor asked")
	}
	got := f.ts.srv.managed.handleSwarmRequest(context.Background(), a.UID, a.Incarnation, inv)
	if !got.OK {
		t.Fatalf("current guest ask: %+v", got)
	}
	f.ts.srv.steps.wait()
	requests, err := f.ts.srv.store.SwarmRequests(context.Background(), f.candidate)
	if err != nil || len(requests) != 1 {
		t.Fatalf("guest question: %+v %v", requests, err)
	}
	decision, err := f.ts.srv.store.GetSwarmEvalReplay(context.Background(), f.candidate, requests[0].ID)
	if err != nil || decision.State != "stopped" || decision.Reason == "" || decision.Answer != nil {
		t.Fatalf("guest question did not durably stop: %+v %v", decision, err)
	}
	sw, err := f.ts.srv.store.GetSwarm(context.Background(), f.candidate)
	if err != nil || sw.Execution != "stopped" || requests[0].AnsweredBy != "" || requests[0].Response != nil || requests[0].Outcome != nil {
		t.Fatalf("fake answer or running eval: %+v %+v %v", sw, requests, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.attests) != 1 || len(d.messages) != 1 {
		t.Fatalf("replay caused guest effects: attests=%d messages=%d", len(d.attests), len(d.messages))
	}
}

// BUG: Entry policy открывает вопрос после начального replay того же прохода.
// HYPOTHESIS: После обычной policy reservation нет повторного replay.
// SOLUTION: Отложенный replay после выхода из effect-методов и их gates.
// RESULT: FIXED — долговечная остановка без одобрения и вызовов гостя.
// STATUS: FIXED
func TestEvalPolicyCreatedDuringDispatchStopsWithoutApproval(t *testing.T) {
	f, d, _ := evalActorDispatchFixture(t, true)
	f.post(t, "/eval/start", nil, 200)
	f.ts.srv.steps.wait()
	ctx := context.Background()
	candidate, err := f.ts.srv.store.GetPipelinePolicyCandidate(ctx, f.candidate, 1, "entry")
	if err != nil {
		t.Fatal(err)
	}
	decision, err := f.ts.srv.store.GetSwarmEvalReplay(ctx, f.candidate, "policy:"+candidate.ID)
	if err != nil || decision.State != "stopped" || decision.Reason == "" || decision.Answer != nil {
		t.Fatalf("new policy question never replayed: %+v %v", decision, err)
	}
	sw, err := f.ts.srv.store.GetSwarm(ctx, f.candidate)
	if err != nil || sw.Execution != "stopped" {
		t.Fatalf("policy question left eval active: %+v %v", sw, err)
	}
	requests, err := f.ts.srv.store.SwarmRequests(ctx, f.candidate)
	if err != nil || len(requests) != 1 || requests[0].AnsweredBy != "" || requests[0].Response != nil {
		t.Fatalf("policy got fake approval: %+v %v", requests, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.attests) != 0 || len(d.messages) != 0 {
		t.Fatalf("policy stop invoked guest: attests=%d messages=%d", len(d.attests), len(d.messages))
	}
}

// BUG: Безопасный отказ Store превращается в HTTP 500 при живом ответе.
// HYPOTHESIS: swarmRequestError не обрабатывает ErrSwarmEvalConflict.
// SOLUTION: Явное отображение отказа в HTTP 409 без изменения Store.
// RESULT: FIXED — ответ 409, вопрос остаётся открытым без выдуманного ответа.
// STATUS: FIXED
func TestEvalLiveAnswerReturnsConflict(t *testing.T) {
	f := evalAPISetup(t)
	f.ts.srv.steps.close()
	f.post(t, "/eval", f.input, http.StatusOK)
	f.post(t, "/eval/start", nil, http.StatusOK)
	requests, err := f.ts.srv.store.SwarmRequests(context.Background(), f.candidate)
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %v %v", requests, err)
	}
	r, body, _ := f.ts.do(f.alice, "POST", "/api/swarms/"+f.candidate+"/requests/"+requests[0].ID+"/answer", f.ts.csrf(f.alice), map[string]any{"outcome": "success"})
	expectStatus(t, "live eval answer refusal", r, body, http.StatusConflict)
	req, err := f.ts.srv.store.GetSwarmRequest(context.Background(), f.candidate, requests[0].ID)
	if err != nil || req.State != "open" || req.AnsweredBy != "" {
		t.Fatalf("refused answer mutated request: %+v %v", req, err)
	}
}
