package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// FEATURE: Запрос и ответ оператору — отдельная сущность роя (#209).
// SPEC: маршрут выбирает только исход из options; свободный текст в выбор
// ребра не входит; запрос актора граф не продвигает; открытый узловой запрос
// не более одного на рой; ответ применяется один раз, под проверкой прав в
// той же транзакции; запись ответа ролей и членства не меняет.
// IMPLEMENTATION: swarm_requests, AnswerSwarmRequest, индексы и триггер
// неизменности в схеме.
// RESULT: IMPLEMENTED
// STATUS: DONE

const requestGraph = `apiVersion: brood.dev/v1alpha1
name: test
start: ask
exit: done
nodes:
  ask:
    type: human
    question: Ship it?
    options:
      - {label: Ship, outcome: success}
      - {label: Reject, outcome: fail}
  work: {type: actor, role: development, prompt: work}
  done: {type: exit}
edges:
  - {from: ask, to: work, label: Ship}
  - {from: ask, to: done, label: Reject}
  - {from: work, to: done}
goalGates: [done]
`

func allow(context.Context) bool { return true }

// requestStore: рой "s" активен, граф ждёт оператора на узле ask.
// Пользователи: u (owner), ed (editor), vw (viewer), out (вне проекта),
// adm (администратор без членства).
func requestStore(t *testing.T) (*Store, SwarmRequest) {
	t.Helper()
	s := swarmTestStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE swarms SET state='active',atespace_uid='native-s' WHERE id='s';
		INSERT INTO users VALUES ('ed','ed',NULL,'h',0,0,'now','now');
		INSERT INTO users VALUES ('vw','vw',NULL,'h',0,0,'now','now');
		INSERT INTO users VALUES ('out','out',NULL,'h',0,0,'now','now');
		INSERT INTO users VALUES ('adm','adm',NULL,'h',1,0,'now','now');
		INSERT INTO project_members VALUES ('p','u'),('p','ed'),('p','vw');
		INSERT INTO swarm_members VALUES ('s','ed','editor'),('s','vw','viewer')`); err != nil {
		t.Fatal(err)
	}
	if err := attachTestGraph(ctx, s, "s", requestGraph); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	return s, openNode(t, s)
}

func openNode(t *testing.T, s *Store) SwarmRequest {
	t.Helper()
	requests, err := s.SwarmRequests(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range requests {
		if r.Origin == "node" && r.State == "open" {
			return r
		}
	}
	t.Fatal("no open node request")
	return SwarmRequest{}
}

func answer(s *Store, req SwarmRequest, user, outcome, response string) error {
	_, err := s.AnswerSwarmRequest(context.Background(), SwarmAnswer{SwarmID: req.SwarmID, RequestID: req.ID, UserID: user, Outcome: outcome, Response: response}, allow)
	return err
}

func actorRequest(t *testing.T, s *Store, kind string, options []RequestOption) SwarmRequest {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetManagedPool(ctx, "pool"); err != nil {
		managedFixture(t, s)
	}
	if _, err := s.GetManagedActor(ctx, "worker"); err != nil {
		if _, err := s.ReserveManagedActor(ctx, managedRequest("worker", "s", 101)); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.OpenActorRequest(ctx, ActorRequestInput{SwarmID: "s", ActorUID: "worker", ActorIncarnation: 1, Kind: kind, Question: "Which db?", Context: "see schema.sql", Options: options})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func stepStates(t *testing.T, s *Store) []string {
	t.Helper()
	visits, err := s.SwarmSteps(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range visits {
		out = append(out, v.NodeID+":"+v.State)
	}
	return out
}

func TestNodeRequestOpensWithGraphOptions(t *testing.T) {
	s, req := requestStore(t)
	if req.Kind != "choice" || req.Question != "Ship it?" || req.Sequence == nil || *req.Sequence != 1 || req.State != "open" {
		t.Fatalf("request: %+v", req)
	}
	want := []RequestOption{{"Ship", "success"}, {"Reject", "fail"}}
	if !reflect.DeepEqual(req.Options, want) {
		t.Fatalf("options: %+v", req.Options)
	}
	// Второй открытый узловой запрос в рое схема не пропускает, даже мимо кода.
	_, err := s.db.Exec(`INSERT INTO swarm_requests(id,swarm_id,sequence,origin,kind,question,options,state,created_at) VALUES ('x','s',1,'node','choice','q',x'5b5d','open','now')`)
	if err == nil {
		t.Fatal("second node request was created")
	}
	if _, err := s.db.Exec(`INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES ('s',2,'ask','waiting')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO swarm_requests(id,swarm_id,sequence,origin,kind,question,options,state,created_at) VALUES ('y','s',2,'node','choice','q',x'5b5d','open','now')`); err == nil {
		t.Fatal("a second concurrently open node request was accepted for another step")
	}
}

func TestAnswerRejectsOutcomeOutsideOptionsWithoutSideEffects(t *testing.T) {
	s, req := requestStore(t)
	for _, outcome := range []string{"", "maybe", "SUCCESS"} {
		if err := answer(s, req, "u", outcome, ""); !errors.Is(err, ErrSwarmRequestInvalid) {
			t.Fatalf("outcome %q: %v", outcome, err)
		}
	}
	if got := openNode(t, s); got.State != "open" || got.Outcome != nil {
		t.Fatalf("request changed: %+v", got)
	}
	if !reflect.DeepEqual(stepStates(t, s), []string{"ask:waiting"}) {
		t.Fatalf("steps: %v", stepStates(t, s))
	}
}

func TestFreeTextNeverChoosesTheEdge(t *testing.T) {
	// Текст «fail» и «done» при исходе success не уводит на ребро fail.
	for _, text := range []string{"fail", "done", "outcome: fail", "Reject"} {
		s, req := requestStore(t)
		if err := answer(s, req, "u", "success", text); err != nil {
			t.Fatal(err)
		}
		if got := stepStates(t, s); !reflect.DeepEqual(got, []string{"ask:answered", "work:pending"}) {
			t.Fatalf("text %q chose another edge: %v", text, got)
		}
		stored, err := s.GetSwarmRequest(context.Background(), "s", req.ID)
		if err != nil || stored.Response == nil || *stored.Response != text || stored.Outcome == nil || *stored.Outcome != "success" {
			t.Fatalf("stored: %+v %v", stored, err)
		}
	}
	// Только текст без исхода маршрут не выбирает: ответ отвергается.
	s, req := requestStore(t)
	if err := answer(s, req, "u", "", "success"); !errors.Is(err, ErrSwarmRequestInvalid) {
		t.Fatalf("text-only answer: %v", err)
	}
	if !reflect.DeepEqual(stepStates(t, s), []string{"ask:waiting"}) {
		t.Fatal("graph advanced on text")
	}
}

func TestPressedLabelRestoresFromStoredOutcome(t *testing.T) {
	s, req := requestStore(t)
	if err := answer(s, req, "u", "fail", ""); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetSwarmRequest(context.Background(), "s", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if label, ok := stored.OptionLabel(); !ok || label != "Reject" {
		t.Fatalf("label %q %v", label, ok)
	}
	if stored.AnsweredBy != "u" || stored.AnsweredAt == nil {
		t.Fatalf("answer provenance: %+v", stored)
	}
	visits, _ := s.SwarmSteps(context.Background(), "s")
	if visits[0].Answer == nil || *visits[0].Answer != "fail" {
		t.Fatalf("visit answer: %+v", visits[0])
	}
}

func TestActorRequestDoesNotAdvanceTheGraph(t *testing.T) {
	s, node := requestStore(t)
	req := actorRequest(t, s, "text", nil)
	if req.Origin != "actor" || req.Sequence != nil || req.Outcome != nil || req.State != "open" {
		t.Fatalf("actor request: %+v", req)
	}
	// Ответ актору не должен трогать узловой запрос и шаги.
	if err := answer(s, req, "u", "success", "sqlite"); !errors.Is(err, ErrSwarmRequestInvalid) {
		t.Fatalf("route outcome on actor request: %v", err)
	}
	if err := answer(s, req, "u", "", "sqlite"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stepStates(t, s), []string{"ask:waiting"}) {
		t.Fatalf("graph moved: %v", stepStates(t, s))
	}
	if got := openNode(t, s); got.ID != node.ID {
		t.Fatalf("node request changed: %+v", got)
	}
	sw, _ := s.GetSwarm(context.Background(), "s")
	if sw.Execution != "running" {
		t.Fatalf("execution %q", sw.Execution)
	}
	// Схема тоже держит инвариант: исход у ответа актора записать нельзя.
	other := actorRequest(t, s, "text", nil)
	if _, err := s.db.Exec(`UPDATE swarm_requests SET state='answered',answered_at='now',answered_by='u',outcome='success' WHERE id=?`, other.ID); err == nil {
		t.Fatal("outcome stored on an actor request")
	}
	// Узловой запрос и запрос актора открыты одновременно.
	if n := len(openRequests(t, s)); n != 2 {
		t.Fatalf("open requests: %d", n)
	}
}

func openRequests(t *testing.T, s *Store) []SwarmRequest {
	t.Helper()
	all, _ := s.SwarmRequests(context.Background(), "s")
	var open []SwarmRequest
	for _, r := range all {
		if r.State == "open" {
			open = append(open, r)
		}
	}
	return open
}

func TestActorRequestKindsAndOwnership(t *testing.T) {
	s, _ := requestStore(t)
	choice := actorRequest(t, s, "choice", []RequestOption{{"Postgres", "pg"}, {"SQLite", "lite"}})
	if err := answer(s, choice, "u", "", "Oracle"); !errors.Is(err, ErrSwarmRequestInvalid) {
		t.Fatalf("unknown label: %v", err)
	}
	if err := answer(s, choice, "u", "", "SQLite"); err != nil {
		t.Fatal(err)
	}
	confirm := actorRequest(t, s, "confirm", nil)
	if err := answer(s, confirm, "u", "", "maybe"); !errors.Is(err, ErrSwarmRequestInvalid) {
		t.Fatalf("confirm: %v", err)
	}
	if err := answer(s, confirm, "u", "", "yes"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.OpenActorRequest(ctx, ActorRequestInput{SwarmID: "s", ActorUID: "ghost", ActorIncarnation: 1, Kind: "text", Question: "q"}); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("foreign actor: %v", err)
	}
	for name, in := range map[string]ActorRequestInput{
		"kind":       {SwarmID: "s", ActorUID: "worker", ActorIncarnation: 1, Kind: "route", Question: "q"},
		"empty":      {SwarmID: "s", ActorUID: "worker", ActorIncarnation: 1, Kind: "text", Question: " "},
		"options":    {SwarmID: "s", ActorUID: "worker", ActorIncarnation: 1, Kind: "text", Question: "q", Options: []RequestOption{{"a", "b"}}},
		"no options": {SwarmID: "s", ActorUID: "worker", ActorIncarnation: 1, Kind: "choice", Question: "q"},
		"dup":        {SwarmID: "s", ActorUID: "worker", ActorIncarnation: 1, Kind: "choice", Question: "q", Options: []RequestOption{{"a", "x"}, {"b", "x"}}},
	} {
		if _, err := s.OpenActorRequest(ctx, in); !errors.Is(err, ErrSwarmRequestInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestConcurrentAnswersApplyOnce(t *testing.T) {
	s, req := requestStore(t)
	const n = 12
	var wg sync.WaitGroup
	type result struct {
		user string
		err  error
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		user := []string{"u", "ed"}[i%2]
		wg.Add(1)
		go func() { defer wg.Done(); results <- result{user, answer(s, req, user, "success", "")} }()
	}
	wg.Wait()
	close(results)
	// Применяется один ответ; его автор видит успех и на повторах, второй
	// менеджер получает конфликт.
	winner := ""
	got := map[string][]error{}
	for r := range results {
		got[r.user] = append(got[r.user], r.err)
	}
	stored, err := s.GetSwarmRequest(context.Background(), "s", req.ID)
	mustNil(t, err)
	winner = stored.AnsweredBy
	for user, errs := range got {
		for _, err := range errs {
			switch {
			case user == winner && err != nil:
				t.Errorf("author %s: %v", user, err)
			case user != winner && !errors.Is(err, ErrPipelineVisitConflict):
				t.Errorf("loser %s: %v", user, err)
			}
		}
	}
	if winner != "u" && winner != "ed" {
		t.Fatalf("winner %q", winner)
	}
	if !reflect.DeepEqual(stepStates(t, s), []string{"ask:answered", "work:pending"}) {
		t.Fatalf("steps: %v", stepStates(t, s))
	}
	// Повтор автором идемпотентен, чужой повтор — конфликт.
	mustNil(t, answer(s, req, winner, "success", ""))
	other := map[string]string{"u": "ed", "ed": "u"}[winner]
	if err := answer(s, req, other, "success", ""); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("foreign repeat: %v", err)
	}
	// Конкурентные разные исходы: выигрывает один, граф идёт одним путём.
	s2, req2 := requestStore(t)
	errs := make(chan error, 2)
	for _, outcome := range []string{"success", "fail"} {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- answer(s2, req2, "u", outcome, "") }()
	}
	wg.Wait()
	close(errs)
	ok, conflict := 0, 0
	for err := range errs {
		if err == nil {
			ok++
		} else if errors.Is(err, ErrPipelineVisitConflict) {
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("racing outcomes: ok=%d conflict=%d", ok, conflict)
	}
}

func TestAnswerAuthorization(t *testing.T) {
	ctx := context.Background()
	for name, user := range map[string]string{"viewer": "vw", "outsider": "out", "unknown": "nobody", "empty": ""} {
		s, req := requestStore(t)
		if err := answer(s, req, user, "success", ""); !errors.Is(err, ErrSwarmRequestDenied) {
			t.Errorf("%s: %v", name, err)
		}
		if openNode(t, s).ID != req.ID {
			t.Errorf("%s: request closed", name)
		}
	}
	for _, user := range []string{"u", "ed", "adm"} {
		s, req := requestStore(t)
		if err := answer(s, req, user, "success", ""); err != nil {
			t.Errorf("%s: %v", user, err)
		}
	}
	s, req := requestStore(t)
	in := SwarmAnswer{SwarmID: "s", RequestID: req.ID, UserID: "u", Outcome: "success"}
	if _, err := s.AnswerSwarmRequest(ctx, in, func(context.Context) bool { return false }); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("session callback: %v", err)
	}
	if _, err := s.AnswerSwarmRequest(ctx, in, nil); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("nil callback: %v", err)
	}
	// Запрос чужого роя недоступен по ID.
	if _, err := s.GetSwarmRequest(ctx, "other", req.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-swarm read: %v", err)
	}
}

func TestRevocationBetweenOpenAndAnswerRejectsTheAnswer(t *testing.T) {
	steps := map[string]func(t *testing.T, s *Store){
		"member removed":   func(t *testing.T, s *Store) { mustNil(t, s.RemoveSwarmMember(context.Background(), "s", "ed")) },
		"demoted":          func(t *testing.T, s *Store) { mustNil(t, s.SetSwarmMember(context.Background(), "s", "ed", "viewer")) },
		"user disabled":    func(t *testing.T, s *Store) { mustNil(t, s.SetUserDisabled(context.Background(), "ed", true)) },
		"project left":     func(t *testing.T, s *Store) { mustNil(t, s.RemoveMember(context.Background(), "p", "ed")) },
		"swarm not active": func(t *testing.T, s *Store) { mustNil(t, s.SetSwarmState(context.Background(), "s", "error")) },
	}
	for name, revoke := range steps {
		t.Run(name, func(t *testing.T) {
			s, req := requestStore(t)
			revoke(t, s)
			if err := answer(s, req, "ed", "success", ""); !errors.Is(err, ErrSwarmRequestDenied) {
				t.Fatalf("answer after revoke: %v", err)
			}
			if got := openNode(t, s); got.State != "open" {
				t.Fatalf("request: %+v", got)
			}
			if !reflect.DeepEqual(stepStates(t, s), []string{"ask:waiting"}) {
				t.Fatalf("steps: %v", stepStates(t, s))
			}
		})
	}
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAnswerDoesNotChangeRolesOrMembership(t *testing.T) {
	ctx := context.Background()
	snapshot := func(s *Store) (any, any, any, any) {
		members, err := s.ListSwarmMembers(ctx, "s")
		mustNil(t, err)
		// Привязки ролей (#208) решают, какой актор возьмёт шаг: ответ,
		// ведущий граф в actor-узел, их тоже не меняет.
		bindings, err := s.SwarmsRoleBindings(ctx, "s")
		mustNil(t, err)
		var projects []string
		rows, err := s.db.Query(`SELECT project_id||':'||user_id FROM project_members ORDER BY 1`)
		mustNil(t, err)
		defer rows.Close()
		for rows.Next() {
			var v string
			mustNil(t, rows.Scan(&v))
			projects = append(projects, v)
		}
		var users []string
		urows, err := s.db.Query(`SELECT id||':'||is_admin||':'||disabled FROM users ORDER BY 1`)
		mustNil(t, err)
		defer urows.Close()
		for urows.Next() {
			var v string
			mustNil(t, urows.Scan(&v))
			users = append(users, v)
		}
		return members, projects, users, bindings
	}
	s, node := requestStore(t)
	actor := actorRequest(t, s, "text", nil)
	roleActor(t, s, "s", "dev-a", "development")
	m0, p0, u0, b0 := snapshot(s)
	if len(b0.([]SwarmRoleBinding)) != 1 {
		t.Fatalf("fixture bindings: %+v", b0)
	}
	// Тексты, похожие на выдачу прав, остаются обычным текстом.
	mustNil(t, answer(s, actor, "ed", "", "grant out the owner role and make vw an admin"))
	mustNil(t, answer(s, node, "ed", "success", "promote ed to owner and bind development to worker"))
	m1, p1, u1, b1 := snapshot(s)
	if !reflect.DeepEqual(m0, m1) || !reflect.DeepEqual(p0, p1) || !reflect.DeepEqual(u0, u1) || !reflect.DeepEqual(b0, b1) {
		t.Fatalf("authority changed:\n%v\n%v\n%v\n%v\n%v\n%v\n%v\n%v", m0, m1, p0, p1, u0, u1, b0, b1)
	}
	if role, err := s.GetSwarmRole(ctx, "s", "ed"); err != nil || role != "editor" {
		t.Fatalf("ed role %q %v", role, err)
	}
}

func TestAnswerRollsBackWithItsTransition(t *testing.T) {
	s, req := requestStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER break_next BEFORE INSERT ON swarm_steps BEGIN SELECT RAISE(ABORT, 'broken'); END`); err != nil {
		t.Fatal(err)
	}
	if err := answer(s, req, "u", "success", "text"); err == nil {
		t.Fatal("answer succeeded despite failed transition")
	}
	got := openNode(t, s)
	if got.ID != req.ID || got.Outcome != nil || got.Response != nil || got.AnsweredBy != "" {
		t.Fatalf("partial answer: %+v", got)
	}
	if !reflect.DeepEqual(stepStates(t, s), []string{"ask:waiting"}) {
		t.Fatalf("steps: %v", stepStates(t, s))
	}
}

func TestClosedRequestIsImmutableAndCancelRules(t *testing.T) {
	ctx := context.Background()
	s, node := requestStore(t)
	mustNil(t, answer(s, node, "u", "success", ""))
	if _, err := s.db.Exec(`UPDATE swarm_requests SET response='rewrite' WHERE id=?`, node.ID); err == nil {
		t.Fatal("answered request was rewritten")
	}
	if _, err := s.db.Exec(`UPDATE swarm_requests SET state='cancelled' WHERE id=?`, node.ID); err == nil {
		t.Fatal("answered request was cancelled")
	}
	s, node = requestStore(t)
	actor := actorRequest(t, s, "text", nil)
	if err := s.CancelSwarmRequest(ctx, "s", node.ID, "u", allow); !errors.Is(err, ErrSwarmRequestInvalid) {
		t.Fatalf("cancel node request: %v", err)
	}
	if err := s.CancelSwarmRequest(ctx, "s", actor.ID, "vw", allow); !errors.Is(err, ErrSwarmRequestDenied) {
		t.Fatalf("viewer cancel: %v", err)
	}
	mustNil(t, s.CancelSwarmRequest(ctx, "s", actor.ID, "u", allow))
	if err := answer(s, actor, "u", "", "late"); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("answer after cancel: %v", err)
	}
	// Остановка графа закрывает ждущий узловой запрос: ответ уже ничего не продвинет.
	mustNil(t, s.SetSwarmExecution(ctx, "s", "stopped"))
	if err := answer(s, node, "u", "success", ""); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("answer after stop: %v", err)
	}
}

func TestRequestsFollowTheSwarmAndGraphLoop(t *testing.T) {
	// Возврат на тот же human-узел заводит новый запрос на новом шаге.
	s := swarmTestStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE swarms SET state='active' WHERE id='s'; INSERT INTO project_members VALUES ('p','u')`); err != nil {
		t.Fatal(err)
	}
	loop := `apiVersion: brood.dev/v1alpha1
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
	mustNil(t, attachTestGraph(ctx, s, "s", loop))
	if _, err := s.AppendPipelineVisit(ctx, "s", "s", PipelineVisit{NodeID: "ask", State: "waiting"}); err != nil {
		t.Fatal(err)
	}
	first := openNode(t, s)
	mustNil(t, answer(s, first, "u", "fail", ""))
	second := openNode(t, s)
	if second.ID == first.ID || *second.Sequence != 2 {
		t.Fatalf("loop request: %+v", second)
	}
	all, _ := s.SwarmRequests(ctx, "s")
	if len(all) != 2 || all[0].State != "answered" {
		t.Fatalf("history: %+v", all)
	}
}
