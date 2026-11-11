package server

// FEATURE: Swarm requests are read and answered through the swarm API, and an
// answer reaches the asking actor over the existing operator channel (#210).
// SPEC: only users who may manage the swarm answer; a request is addressed
// within its own swarm; the answer commits before delivery, a delivery failure
// keeps the answer and is retried on the operator's request without moving the
// graph again; an actor asks only as its current incarnation and cannot answer
// through the guest path; clients see only fixed public error messages.
// IMPLEMENTATION: swarm_requests_api.go, swarm_requests_guest.go,
// deliverOperatorMessage shared with send.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/substrate"
)

// requestServer: alice — администратор и владелец роя с master и worker;
// bob — участник проекта без роли в рое. deliveries считает сообщения
// актору, fail переключает доставку в отказ гостя до записи в сессию
// (notSent), reply, если задан, отвечает вместо гостя. budgets — сколько
// времени сервер оставил гостю на каждое сообщение.
type requestServer struct {
	*substrateServer
	alice, bob *http.Client
	master     store.ManagedActor
	worker     store.ManagedActor
	deliveries atomic.Int32
	fail       atomic.Bool
	mu         sync.Mutex
	messages   []relay.MessageRequest
	budgets    []time.Duration
	reply      func() (relay.MessageResponse, error)
}

func newRequestServer(t *testing.T) *requestServer {
	t.Helper()
	ts, master, worker := coordinationServerSetup(t)
	rs := &requestServer{substrateServer: ts, master: master, worker: worker, alice: newClient(t), bob: newClient(t)}
	ts.login(rs.alice, "alice", "alice-password-1")
	ts.login(rs.bob, "bob", "bob-password-1")
	ts.srv.managed.deliverMessage = func(ctx context.Context, uid string, generation int64, req relay.MessageRequest) (relay.MessageResponse, error) {
		rs.deliveries.Add(1)
		if uid != worker.UID || generation != worker.Incarnation {
			t.Errorf("answer delivered to %s/%d, want %s/%d", uid, generation, worker.UID, worker.Incarnation)
		}
		var budget time.Duration
		if deadline, ok := ctx.Deadline(); ok {
			budget = time.Until(deadline)
		}
		rs.mu.Lock()
		rs.messages = append(rs.messages, req)
		rs.budgets = append(rs.budgets, budget)
		reply := rs.reply
		rs.mu.Unlock()
		switch {
		case reply != nil:
			return reply()
		case rs.fail.Load():
			return sessionNotReady, nil
		}
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	return rs
}

// ask открывает вопрос актора гостевым путём и возвращает ID запроса.
func (rs *requestServer) ask(t *testing.T, actor store.ManagedActor, fields map[string]any) (relay.SwarmResponse, string) {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	got := rs.srv.managed.handleSwarmRequest(context.Background(), actor.UID, actor.Incarnation, relay.SwarmRequest{Tool: "ask_operator", Arguments: data})
	var opened struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(got.Data, &opened)
	return got, opened.RequestID
}

func (rs *requestServer) mustAsk(t *testing.T, fields map[string]any) string {
	t.Helper()
	got, id := rs.ask(t, rs.worker, fields)
	if !got.OK || id == "" {
		t.Fatalf("ask_operator: %+v", got)
	}
	return id
}

func (rs *requestServer) request(t *testing.T, id string) store.SwarmRequest {
	t.Helper()
	req, err := rs.srv.store.GetSwarmRequest(context.Background(), rs.master.SwarmID, id)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func (rs *requestServer) answerPath(swarm, id string) string {
	return "/api/swarms/" + swarm + "/requests/" + id + "/answer"
}

func (rs *requestServer) list(t *testing.T, client *http.Client, query string) []map[string]any {
	t.Helper()
	r, b, raw := rs.do(client, "GET", "/api/swarms/"+rs.master.SwarmID+"/requests"+query, "", nil)
	expectStatus(t, "list "+query, r, b, 200)
	var body struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil || body.Results == nil {
		t.Fatalf("list body: %s %v", raw, err)
	}
	return body.Results
}

func TestSwarmRequestListAnswerAndDelivery(t *testing.T) {
	rs := newRequestServer(t)
	swarm := rs.master.SwarmID
	id := rs.mustAsk(t, map[string]any{"question": "Which database?", "context": "see schema.sql"})
	open := rs.list(t, rs.alice, "")
	if len(open) != 1 || open[0]["id"] != id || open[0]["origin"] != "actor" || open[0]["kind"] != "text" || open[0]["state"] != "open" || open[0]["question"] != "Which database?" || open[0]["context"] != "see schema.sql" {
		t.Fatalf("open requests: %v", open)
	}
	if actor, _ := open[0]["actor"].(map[string]any); actor["name"] != "worker" || actor["incarnation"] != float64(rs.worker.Incarnation) {
		t.Fatalf("request actor: %v", open[0]["actor"])
	}
	r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(swarm, id), rs.csrf(rs.alice), map[string]any{"response": "use sqlite"})
	expectStatus(t, "answer", r, b, 200)
	if b["state"] != "answered" || b["response"] != "use sqlite" || b["answeredBy"] != "alice" || b["delivery"] != "delivered" || b["deliveryError"] != nil {
		t.Fatalf("answer: %v", b)
	}
	if rs.deliveries.Load() != 1 {
		t.Fatalf("deliveries=%d", rs.deliveries.Load())
	}
	message := rs.messages[0]
	if message.To != "worker" || message.From != "operator:alice" || !strings.Contains(message.Body, id) || !strings.Contains(message.Body, "use sqlite") {
		t.Fatalf("delivered message: %+v", message)
	}
	if got := rs.list(t, rs.alice, ""); len(got) != 0 {
		t.Fatalf("answered request still listed as open: %v", got)
	}
	all := rs.list(t, rs.alice, "?all=1")
	if len(all) != 1 || all[0]["delivery"] != "delivered" || all[0]["answeredBy"] != "alice" {
		t.Fatalf("all requests: %v", all)
	}
	r, b, _ = rs.do(rs.alice, "GET", "/api/swarms/"+swarm+"/requests?all=maybe", "", nil)
	expectStatus(t, "invalid filter", r, b, 400)
}

func TestSwarmRequestAnswerRequiresTheRightToManageTheSwarm(t *testing.T) {
	rs := newRequestServer(t)
	swarm := rs.master.SwarmID
	id := rs.mustAsk(t, map[string]any{"question": "Ship it?", "kind": "confirm"})
	path := rs.answerPath(swarm, id)
	payload := map[string]any{"response": "yes"}
	r, b, _ := rs.do(rs.bob, "GET", "/api/swarms/"+swarm+"/requests", "", nil)
	expectStatus(t, "outsider list", r, b, 404)
	r, b, _ = rs.do(rs.bob, "POST", path, rs.csrf(rs.bob), payload)
	expectStatus(t, "outsider answer", r, b, 404)
	r, b, _ = rs.do(rs.alice, "POST", "/api/swarms/"+swarm+"/members", rs.csrf(rs.alice), map[string]string{"username": "bob", "role": "viewer"})
	expectStatus(t, "add viewer", r, b, 204)
	if got := rs.list(t, rs.bob, ""); len(got) != 1 {
		t.Fatalf("viewer list: %v", got)
	}
	r, b, _ = rs.do(rs.bob, "POST", path, rs.csrf(rs.bob), payload)
	expectStatus(t, "viewer answer", r, b, 404)
	r, b, _ = rs.do(rs.alice, "POST", path, "", payload)
	expectStatus(t, "answer without csrf", r, b, 403)
	r, b, _ = rs.do(newClient(t), "POST", path, "", payload)
	if r.StatusCode == 200 {
		t.Fatalf("anonymous answer accepted: %v", b)
	}
	if req := rs.request(t, id); req.State != "open" || req.AnsweredBy != "" || rs.deliveries.Load() != 0 {
		t.Fatalf("refused answers wrote or delivered: %+v deliveries=%d", req, rs.deliveries.Load())
	}
	r, b, _ = rs.do(rs.alice, "POST", "/api/swarms/"+swarm+"/members", rs.csrf(rs.alice), map[string]string{"username": "bob", "role": "editor"})
	expectStatus(t, "make editor", r, b, 204)
	r, b, _ = rs.do(rs.bob, "POST", path, rs.csrf(rs.bob), payload)
	expectStatus(t, "editor answer", r, b, 200)
}

func TestSwarmRequestIsAnsweredOnlyThroughItsOwnSwarm(t *testing.T) {
	rs := newRequestServer(t)
	ctx := context.Background()
	swarm := rs.master.SwarmID
	sw, err := rs.srv.store.GetSwarm(ctx, swarm)
	if err != nil {
		t.Fatal(err)
	}
	r, b, _ := rs.do(rs.alice, "POST", "/api/projects/"+sw.ProjectID+"/swarms", rs.csrf(rs.alice), map[string]string{"name": "other"})
	expectStatus(t, "other swarm", r, b, http.StatusCreated)
	other, _ := b["id"].(string)
	id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
	// alice управляет обоими роями, но запрос адресуется только своим роем.
	r, b, _ = rs.do(rs.alice, "POST", rs.answerPath(other, id), rs.csrf(rs.alice), map[string]any{"response": "sqlite"})
	expectStatus(t, "foreign swarm answer", r, b, 404)
	r, b, _ = rs.do(rs.alice, "POST", "/api/swarms/"+other+"/requests/"+id+"/deliver", rs.csrf(rs.alice), nil)
	expectStatus(t, "foreign swarm delivery", r, b, 404)
	r, b, raw := rs.do(rs.alice, "GET", "/api/swarms/"+other+"/requests?all=1", "", nil)
	expectStatus(t, "foreign list", r, b, 200)
	if strings.Contains(raw, id) {
		t.Fatalf("request listed in another swarm: %s", raw)
	}
	if req := rs.request(t, id); req.State != "open" || rs.deliveries.Load() != 0 {
		t.Fatalf("foreign path answered: %+v", req)
	}
}

func TestSwarmAnswerDeliveryFailureKeepsTheAnswerAndRetryKeepsTheGraph(t *testing.T) {
	rs := newRequestServer(t)
	ctx := context.Background()
	swarm := rs.master.SwarmID
	if err := insertSwarmGraph(ctx, rs.testServer, swarm, []byte(pipelineAPIGraph), "waiting"); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.srv.store.AppendPipelineVisit(ctx, swarm, swarm, store.PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	var node store.SwarmRequest
	requests, err := rs.srv.store.SwarmRequests(ctx, swarm)
	if err != nil || len(requests) != 1 {
		t.Fatalf("node request: %+v %v", requests, err)
	}
	node = requests[0]
	r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(swarm, node.ID), rs.csrf(rs.alice), map[string]any{"outcome": "success", "response": "ship it"})
	expectStatus(t, "node answer", r, b, 200)
	if b["state"] != "answered" || b["outcome"] != "success" || b["optionLabel"] != "yes" || b["delivery"] != nil || rs.deliveries.Load() != 0 {
		t.Fatalf("node answer: %v deliveries=%d", b, rs.deliveries.Load())
	}
	steps, err := rs.srv.store.SwarmSteps(ctx, swarm)
	if err != nil || len(steps) != 2 || steps[1].NodeID != "done" {
		t.Fatalf("node answer did not advance once: %+v %v", steps, err)
	}

	rs.fail.Store(true)
	id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
	r, b, raw := rs.do(rs.alice, "POST", rs.answerPath(swarm, id), rs.csrf(rs.alice), map[string]any{"response": "use sqlite"})
	expectStatus(t, "answer with failed delivery", r, b, 200)
	if b["state"] != "answered" || b["delivery"] != "failed" || b["deliveryError"] != "the answer was not sent: the actor session did not receive it" || strings.Contains(raw, sessionNotReady.Verdict) {
		t.Fatalf("failed delivery: %s", raw)
	}
	if req := rs.request(t, id); req.State != "answered" || req.Response == nil || *req.Response != "use sqlite" || req.Delivery != "failed" {
		t.Fatalf("failed delivery rolled back the answer: %+v", req)
	}

	deliver := "/api/swarms/" + swarm + "/requests/" + id + "/deliver"
	r, b, _ = rs.do(rs.alice, "POST", deliver, rs.csrf(rs.alice), nil)
	expectStatus(t, "failed retry", r, b, 200)
	if b["delivery"] != "failed" || rs.deliveries.Load() != 2 {
		t.Fatalf("failed retry: %v deliveries=%d", b, rs.deliveries.Load())
	}
	rs.fail.Store(false)
	r, b, _ = rs.do(rs.alice, "POST", deliver, rs.csrf(rs.alice), nil)
	expectStatus(t, "retry", r, b, 200)
	if b["delivery"] != "delivered" || b["deliveryError"] != nil || rs.deliveries.Load() != 3 {
		t.Fatalf("retry: %v deliveries=%d", b, rs.deliveries.Load())
	}
	r, b, _ = rs.do(rs.alice, "POST", deliver, rs.csrf(rs.alice), nil)
	expectStatus(t, "delivered retry", r, b, 200)
	if b["delivery"] != "delivered" || rs.deliveries.Load() != 3 {
		t.Fatalf("delivered answer was sent again: %v deliveries=%d", b, rs.deliveries.Load())
	}
	r, b, _ = rs.do(rs.alice, "POST", "/api/swarms/"+swarm+"/requests/"+node.ID+"/deliver", rs.csrf(rs.alice), nil)
	expectStatus(t, "node delivery", r, b, 409)
	after, err := rs.srv.store.SwarmSteps(ctx, swarm)
	if err != nil || !reflect.DeepEqual(after, steps) {
		t.Fatalf("delivery moved the graph:\n got %+v\nwant %+v", after, steps)
	}
	if got := rs.request(t, node.ID); got.State != "answered" || *got.Outcome != "success" {
		t.Fatalf("node request changed: %+v", got)
	}
	r, b, _ = rs.do(rs.alice, "POST", "/api/swarms/"+swarm+"/members", rs.csrf(rs.alice), map[string]string{"username": "bob", "role": "viewer"})
	expectStatus(t, "add viewer", r, b, 204)
	r, b, _ = rs.do(rs.bob, "POST", deliver, rs.csrf(rs.bob), nil)
	expectStatus(t, "viewer retry", r, b, 404)
}

// unconfirmedReason — причина неизвестного исхода доставки ответа (#260).
const unconfirmedReason = "delivery is unconfirmed: the answer may have reached the actor, and a retry may deliver it twice"

// TestSwarmAnswerDeliveryOutcomes — исход доставки ответа по ответу гостя
// (#260): совпавшая квитанция — delivered; запись без квитанции и обрыв
// канала после отправки — unconfirmed, ответ мог дойти, повтор может дать
// дубль; отказ гостя до записи в сессию (notSent) — failed, ответ заведомо
// не ушёл. Причина — фиксированный текст, вердикт гостя и транспорт клиенту
// не уходят; список показывает то же состояние.
func TestSwarmAnswerDeliveryOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name             string
		reply            relay.MessageResponse
		err              error
		delivery, reason string
	}{
		{"delivered", relay.MessageResponse{OK: true, Verdict: "delivered"}, nil, "delivered", ""},
		{"receipt unconfirmed", relay.MessageResponse{Verdict: "submitted; delivery unconfirmed"}, nil, "unconfirmed", unconfirmedReason},
		{"channel failed after the send", relay.MessageResponse{}, errors.New("private-transport-sentinel"), "unconfirmed", unconfirmedReason},
		{"not sent", sessionNotReady, nil, "failed", "the answer was not sent: the actor session did not receive it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRequestServer(t)
			rs.reply = func() (relay.MessageResponse, error) { return tc.reply, tc.err }
			id := rs.mustAsk(t, map[string]any{"question": "Which identity?"})
			r, b, raw := rs.do(rs.alice, "POST", rs.answerPath(rs.master.SwarmID, id), rs.csrf(rs.alice), map[string]any{"response": "ermak"})
			expectStatus(t, "answer", r, b, 200)
			reason, _ := b["deliveryError"].(string)
			if b["state"] != "answered" || b["delivery"] != tc.delivery || reason != tc.reason {
				t.Fatalf("answer: %s", raw)
			}
			for _, private := range []string{"private-transport-sentinel", sessionNotReady.Verdict, "submitted;"} {
				if strings.Contains(raw, private) {
					t.Fatalf("guest or transport text reached the client: %s", raw)
				}
			}
			if req := rs.request(t, id); req.Delivery != tc.delivery || rs.deliveries.Load() != 1 {
				t.Fatalf("recorded %+v after %d deliveries", req, rs.deliveries.Load())
			}
			if all := rs.list(t, rs.alice, "?all=1"); len(all) != 1 || all[0]["delivery"] != tc.delivery {
				t.Fatalf("listed: %v", all)
			}
		})
	}
}

// TestUnconfirmedAnswerDeliveryRetry — повтор из unconfirmed разрешён
// (#260): ответ уходит снова, и до подтверждения доставка остаётся
// неизвестной — повтор, заведомо не отправленный, не делает её failed, а
// причина напоминает о возможном дубле. Подтверждённый повтор — delivered,
// дальше ответ не отправляется.
func TestUnconfirmedAnswerDeliveryRetry(t *testing.T) {
	rs := newRequestServer(t)
	swarm := rs.master.SwarmID
	rs.reply = func() (relay.MessageResponse, error) {
		return relay.MessageResponse{Verdict: "submitted; delivery unconfirmed"}, nil
	}
	id := rs.mustAsk(t, map[string]any{"question": "Which identity?"})
	r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(swarm, id), rs.csrf(rs.alice), map[string]any{"response": "ermak"})
	expectStatus(t, "answer", r, b, 200)
	if b["delivery"] != "unconfirmed" || b["deliveryError"] != unconfirmedReason {
		t.Fatalf("unconfirmed answer: %v", b)
	}
	deliver := "/api/swarms/" + swarm + "/requests/" + id + "/deliver"
	rs.reply = func() (relay.MessageResponse, error) { return sessionNotReady, nil }
	r, b, _ = rs.do(rs.alice, "POST", deliver, rs.csrf(rs.alice), nil)
	expectStatus(t, "retry not sent", r, b, 200)
	if b["delivery"] != "unconfirmed" || b["deliveryError"] != "the answer was not sent: the actor session did not receive it; an earlier delivery is unconfirmed: the answer may have reached the actor, and a retry may deliver it twice" || rs.deliveries.Load() != 2 {
		t.Fatalf("retry not sent: %v deliveries=%d", b, rs.deliveries.Load())
	}
	rs.reply = nil
	r, b, _ = rs.do(rs.alice, "POST", deliver, rs.csrf(rs.alice), nil)
	expectStatus(t, "retry", r, b, 200)
	if b["delivery"] != "delivered" || b["deliveryError"] != nil || rs.deliveries.Load() != 3 {
		t.Fatalf("retry: %v deliveries=%d", b, rs.deliveries.Load())
	}
	r, b, _ = rs.do(rs.alice, "POST", deliver, rs.csrf(rs.alice), nil)
	expectStatus(t, "delivered retry", r, b, 200)
	if b["delivery"] != "delivered" || rs.deliveries.Load() != 3 {
		t.Fatalf("delivered answer was sent again: %v deliveries=%d", b, rs.deliveries.Load())
	}
}

// TestAnswerDeliveryLeavesTheGuestItsReceiptWait — сервер ждёт гостя не
// меньше предела канала (#260): гость ждёт квитанцию до
// command.ReceiptWait, и доставка ответа не кончается раньше его вердикта.
func TestAnswerDeliveryLeavesTheGuestItsReceiptWait(t *testing.T) {
	rs := newRequestServer(t)
	id := rs.mustAsk(t, map[string]any{"question": "Which identity?"})
	r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(rs.master.SwarmID, id), rs.csrf(rs.alice), map[string]any{"response": "ermak"})
	expectStatus(t, "answer", r, b, 200)
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.budgets) != 1 || rs.budgets[0] < command.CallTimeout(0) {
		t.Fatalf("guest budget %v, want at least %v", rs.budgets, command.CallTimeout(0))
	}
}

// TestRequestDeliveryOutcome — исход попытки по каналу оператора: только
// подтверждённое сообщение — delivered, заведомо не ушедшее — failed с
// причиной, всё после отправки без подтверждения — unconfirmed.
func TestRequestDeliveryOutcome(t *testing.T) {
	completed := command.Result{Outcome: "completed", Text: "delivered"}
	unknown := command.Result{Outcome: "unconfirmed", Text: "submitted; delivery unconfirmed"}
	for _, tc := range []struct {
		name            string
		result          command.Result
		sent            bool
		err             error
		outcome, reason string
	}{
		{"delivered", completed, true, nil, "delivered", ""},
		{"no receipt", unknown, true, nil, "unconfirmed", unconfirmedReason},
		{"channel failed", command.Result{Outcome: "unconfirmed"}, true, errDeliveryUnconfirmed, "unconfirmed", unconfirmedReason},
		{"authorization changed after the send", command.Result{}, true, app.ErrNotAllowed, "unconfirmed", unconfirmedReason},
		{"not sent", unknown, false, errMessageNotSent, "failed", "the answer was not sent: the actor session did not receive it"},
		{"offline", command.Result{}, false, errRecipientOffline, "failed", "recipient is not connected"},
		{"stale actor", command.Result{}, false, app.ErrNotFound, "failed", "request actor is stale"},
		{"authorization changed before the send", command.Result{}, false, app.ErrNotAllowed, "failed", "delivery authorization changed"},
		{"server failure", command.Result{}, false, errors.New("private"), "failed", "delivery unavailable"},
	} {
		outcome, reason := requestDeliveryOutcome(tc.result, tc.sent, tc.err)
		if outcome != tc.outcome || reason != tc.reason {
			t.Errorf("%s: %s %q, want %s %q", tc.name, outcome, reason, tc.outcome, tc.reason)
		}
	}
}

func TestConcurrentDeliveryRetriesSendOnce(t *testing.T) {
	rs := newRequestServer(t)
	swarm := rs.master.SwarmID
	rs.fail.Store(true)
	id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
	r, b, _ := rs.do(rs.alice, "POST", rs.answerPath(swarm, id), rs.csrf(rs.alice), map[string]any{"response": "use sqlite"})
	expectStatus(t, "answer", r, b, 200)
	entered, release := make(chan struct{}, 4), make(chan struct{})
	var calls atomic.Int32
	rs.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return relay.MessageResponse{OK: true}, nil
	}
	token := rs.csrf(rs.alice)
	deliver := "/api/swarms/" + swarm + "/requests/" + id + "/deliver"
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	retry := func() {
		defer wg.Done()
		r, _, _ := rs.do(rs.alice, "POST", deliver, token, nil)
		statuses <- r.StatusCode
	}
	wg.Add(1)
	go retry()
	<-entered
	wg.Add(1)
	go retry()
	select {
	case <-entered:
		t.Error("a concurrent retry sent the answer again")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != 200 {
			t.Fatalf("retry status %d", status)
		}
	}
	if calls.Load() != 1 || rs.request(t, id).Delivery != "delivered" {
		t.Fatalf("calls=%d delivery=%q", calls.Load(), rs.request(t, id).Delivery)
	}
}

func TestSwarmRequestErrorsArePublicMessages(t *testing.T) {
	private := "private-server-sentinel"
	for _, tc := range []struct {
		err    error
		status int
		detail string
	}{
		{fmt.Errorf("%w: %s", store.ErrNotFound, private), 404, "swarm request not found"},
		{fmt.Errorf("%w: %s", store.ErrSwarmRequestDenied, private), 403, "answer authorization changed or access denied"},
		{fmt.Errorf("%w: %s", app.ErrNotAllowed, private), 403, "answer authorization changed or access denied"},
		{fmt.Errorf("%w: %s", store.ErrSwarmRequestStale, private), 409, "request actor is stale"},
		{fmt.Errorf("%w: %s", store.ErrPipelineVisitConflict, private), 409, "swarm request is no longer open"},
		{fmt.Errorf("%w: %s", store.ErrSwarmRequestInvalid, private), 400, "answer does not fit the request: check outcome and response"},
		{&substrate.APIError{Status: 503, Detail: private}, 500, "internal error"},
		{errors.New(private), 500, "internal error"},
	} {
		w := httptest.NewRecorder()
		swarmRequestError(w, httptest.NewRequest("POST", "/", nil), tc.err)
		var body struct {
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != tc.status || body.Detail != tc.detail {
			t.Errorf("%v: %d %q, want %d %q", tc.err, w.Code, body.Detail, tc.status, tc.detail)
		}
	}
	rs := newRequestServer(t)
	id := rs.mustAsk(t, map[string]any{"question": "Which database?"})
	r, b, raw := rs.do(rs.alice, "POST", rs.answerPath(rs.master.SwarmID, id), rs.csrf(rs.alice), map[string]any{"outcome": "success", "response": "sqlite"})
	expectStatus(t, "outcome on actor request", r, b, 400)
	if b["detail"] != "answer does not fit the request: check outcome and response" || strings.Contains(raw, "route outcome") {
		t.Fatalf("store text reached the client: %s", raw)
	}
	r, b, _ = rs.do(rs.alice, "POST", rs.answerPath(rs.master.SwarmID, id), rs.csrf(rs.alice), map[string]any{"response": "sqlite", "answered_by": "bob"})
	expectStatus(t, "unknown answer field", r, b, 400)
	if b["detail"] != "invalid swarm answer" {
		t.Fatalf("decode refusal: %v", b)
	}
}

func TestGuestAsksOnlyAsItsCurrentIncarnation(t *testing.T) {
	rs := newRequestServer(t)
	ctx := context.Background()
	base := func() map[string]any { return map[string]any{"question": "Which database?"} }
	for _, forged := range []string{"actor_uid", "incarnation", "swarm_id"} {
		args := base()
		args[forged] = rs.master.UID
		if got, _ := rs.ask(t, rs.worker, args); got.OK {
			t.Fatalf("forged %s accepted: %+v", forged, got)
		}
	}
	stale := rs.worker
	stale.Incarnation++
	if got, _ := rs.ask(t, stale, base()); got.OK {
		t.Fatalf("stale incarnation accepted: %+v", got)
	}
	got, id := rs.ask(t, rs.master, base())
	if !got.OK || got.Text != "operator request "+id+" is open; the answer arrives as a message from the operator" {
		t.Fatalf("master ask: %+v", got)
	}
	if req := rs.request(t, id); req.ActorUID != rs.master.UID || req.ActorIncarnation != rs.master.Incarnation || req.Origin != "actor" {
		t.Fatalf("request is attributed to %s/%d, want master", req.ActorUID, req.ActorIncarnation)
	}
	choice := rs.mustAsk(t, map[string]any{"question": "Which database?", "kind": "choice", "options": "Postgres\n SQLite \n\n"})
	if req := rs.request(t, choice); !reflect.DeepEqual(req.Options, []store.RequestOption{{Label: "Postgres", Outcome: "Postgres"}, {Label: "SQLite", Outcome: "SQLite"}}) || req.ActorUID != rs.worker.UID {
		t.Fatalf("choice request: %+v", req)
	}
	if got, _ := rs.ask(t, rs.worker, map[string]any{"question": "q", "kind": "text", "options": "a"}); got.OK || got.Text != "invalid operator request: check kind, options and text size" {
		t.Fatalf("options on text: %+v", got)
	}
	// Личность перепроверяется в транзакции записи: актор остановлен после
	// проверки atespace — вопрос не открывается.
	rs.srv.ateapi = nil
	worker := rs.worker
	rs.srv.provision = &probingProvisioner{uid: worker.AtespaceUID, probe: func() error {
		current, err := rs.srv.store.GetManagedActor(ctx, worker.UID)
		if err != nil {
			return err
		}
		current.Phase = "stopped"
		_, err = rs.srv.store.UpdateManagedActor(ctx, current, current.Revision)
		return err
	}}
	before, err := rs.srv.store.SwarmRequests(ctx, rs.master.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := rs.ask(t, rs.worker, base()); got.OK || got.Text != "operator request not authorized" {
		t.Fatalf("revoked actor asked: %+v", got)
	}
	if after, _ := rs.srv.store.SwarmRequests(ctx, rs.master.SwarmID); len(after) != len(before) {
		t.Fatalf("revoked actor opened a request: %+v", after)
	}
}

func TestGuestCannotAnswerItsOwnRequest(t *testing.T) {
	rs := newRequestServer(t)
	ctx := context.Background()
	id := rs.mustAsk(t, map[string]any{"question": "May I push?", "kind": "confirm"})
	attempts := []relay.SwarmRequest{
		{Tool: "ask_operator", Arguments: json.RawMessage(`{"question":"May I push?","request_id":"` + id + `","response":"yes"}`)},
		{Tool: "answer", Arguments: json.RawMessage(`{"request_id":"` + id + `","response":"yes"}`)},
		{Tool: "swarm_request_answer", Arguments: json.RawMessage(`{"request_id":"` + id + `","response":"yes"}`)},
		{Tool: "pipeline_visit_complete", Arguments: json.RawMessage(`{"run_id":"` + rs.master.SwarmID + `","sequence":1,"request_id":"` + id + `","outcome":"success"}`)},
		{Tool: "send", To: "alice", Message: "yes"},
	}
	for _, attempt := range attempts {
		if got := rs.srv.managed.handleSwarmRequest(ctx, rs.worker.UID, rs.worker.Incarnation, attempt); got.OK {
			t.Fatalf("guest %s succeeded: %+v", attempt.Tool, got)
		}
	}
	// У гостя нет браузерной сессии: маршрут ответа без неё не проходит.
	r, b, _ := rs.do(newClient(t), "POST", rs.answerPath(rs.master.SwarmID, id), "", map[string]any{"response": "yes"})
	if r.StatusCode == 200 {
		t.Fatalf("sessionless answer accepted: %v", b)
	}
	if req := rs.request(t, id); req.State != "open" || req.Response != nil || req.AnsweredBy != "" {
		t.Fatalf("guest answered its own request: %+v", req)
	}
}

func TestRequestAnswerMessage(t *testing.T) {
	question := strings.Repeat("é", store.MaxSwarmRequestTextBytes/2)
	response := strings.Repeat("\x01", store.MaxSwarmRequestTextBytes)
	req := store.SwarmRequest{ID: "req-1", Question: "Which\ndatabase? " + question, Response: &response, Outcome: nil}
	body := operatorMessageBody(requestAnswerMessage(req, "alice"))
	for _, want := range []string{"req-1", "alice", response, "Which database?", "ask_operator"} {
		if !strings.Contains(body, want) {
			t.Errorf("message lacks %q", want[:min(len(want), 20)])
		}
	}
	if !utf8.ValidString(body) || strings.Contains(body, question) {
		t.Fatal("question excerpt is not a bounded UTF-8 prefix")
	}
	frame, err := json.Marshal(relay.MessageRequest{To: "worker", From: "operator:alice", Body: body})
	if err != nil || len(frame) > relay.MaxMessageBytes {
		t.Fatalf("largest answer does not fit a relay frame: %d bytes %v", len(frame), err)
	}
}
