package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Telegram mirror of swarm requests and pass boundaries (#282, epic
// #278).
// SPEC: the transaction of a domain change queues its Telegram notification:
// a node request opened (request_opened), an actor question
// (actor_question), an answer by anyone (request_answered), a changed answer
// delivery (answer_delivery), a pass started (pass_started), stopped or
// finished (pass_finished) and a step refused or left unconfirmed
// (step_failed). A rolled back change leaves no notification, and a change
// whose notification fails to write is rolled back. Each recipient
// can read the swarm at that moment (an enabled administrator, or a project
// member with a role in the swarm), has a live link (linked, with its
// telegram session) and is subscribed to the kind; anyone else gets no row.
// Per user the notifications follow the order of the events. A row carries
// the swarm and the object of the event (ref, payload), not rendered text and
// not the goal of a pass or the reason of a failed step, which stay in the
// step (GetSwarmStep); a pass is named by its first step, at whatever step it
// is stopped or ends.
// IMPLEMENTATION: queueTelegram (telegram_outbox.go), called by
// openNodeRequest, OpenActorRequest, AnswerSwarmRequest,
// RecordSwarmRequestDelivery, beginPass, StopSwarm, progressPipeline,
// refusePendingStep, moveAttempt and RecoverActorSteps.
// STATUS: DONE

// mirrorStore: startStore, где читатели роя «one» — u (owner), v (viewer) и
// root (администратор) — привязали чаты 11, 12 и 13 через бот установки.
// Привязаны и не читают рой: w — участник проекта без роли в рое (чат 14), x
// — вне проекта (чат 15).
func mirrorStore(t *testing.T) *Store {
	t.Helper()
	s := startStore(t)
	seedUser(t, s, "w", "member")
	seedUser(t, s, "x", "outsider")
	mustNil(t, s.AddMember(context.Background(), "one", "w"))
	for i, user := range []string{"u", "v", "root", "w", "x"} {
		linkTelegram(t, s, user, "", int64(11+i))
	}
	return s
}

// queued — уведомления очереди по порядку id как «user:kind:ref».
func queued(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT user_id || ':' || kind || ':' || ref FROM telegram_outbox ORDER BY id`)
	mustNil(t, err)
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var row string
		mustNil(t, rows.Scan(&row))
		out = append(out, row)
	}
	mustNil(t, rows.Err())
	return out
}

// queuedOf — уведомления вида kind по порядку id как «user:ref».
func queuedOf(t *testing.T, s *Store, kind string) []string {
	t.Helper()
	out := []string{}
	for _, row := range queued(t, s) {
		user, rest, _ := strings.Cut(row, ":")
		if k, ref, _ := strings.Cut(rest, ":"); k == kind {
			out = append(out, user+":"+ref)
		}
	}
	return out
}

// eventsOf — данные уведомлений вида kind пользователя user по порядку id.
func eventsOf(t *testing.T, s *Store, user, kind string) []TelegramEvent {
	t.Helper()
	rows, err := s.db.Query(`SELECT payload FROM telegram_outbox WHERE user_id = ? AND kind = ? ORDER BY id`, user, kind)
	mustNil(t, err)
	defer rows.Close()
	out := []TelegramEvent{}
	for rows.Next() {
		var payload string
		mustNil(t, rows.Scan(&payload))
		var ev TelegramEvent
		mustNil(t, json.Unmarshal([]byte(payload), &ev))
		out = append(out, ev)
	}
	mustNil(t, rows.Err())
	return out
}

// readers — строки «user:ref» читателей роя «one» с привязкой, по id
// пользователя: в этом порядке их вставляет одно событие.
func readers(ref string) []string {
	return []string{"root:" + ref, "u:" + ref, "v:" + ref}
}

func openRequestOf(t *testing.T, s *Store, origin string) SwarmRequest {
	t.Helper()
	requests, err := s.SwarmRequests(context.Background(), "one")
	mustNil(t, err)
	for _, r := range requests {
		if r.Origin == origin && r.State == "open" {
			return r
		}
	}
	t.Fatalf("no open %s request", origin)
	return SwarmRequest{}
}

func sameStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestPassStartQueuesItsBoundaryAndRequestForLinkedReaders(t *testing.T) {
	s := mirrorStore(t)
	mustStart(t, s, "approve")
	req := openRequestOf(t, s, "node")
	request := "swarm/one/request/" + req.ID
	// Каждому читателю — начало прохода раньше запроса его первого шага;
	// w и x рой не читают.
	sameStrings(t, "queue after the start", queued(t, s), []string{
		"root:pass_started:swarm/one/pass/1", "u:pass_started:swarm/one/pass/1", "v:pass_started:swarm/one/pass/1",
		"root:request_opened:" + request, "u:request_opened:" + request, "v:request_opened:" + request,
	})
	if got := eventsOf(t, s, "v", "pass_started"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Sequence: 1, Node: "ask", Execution: "waiting"}}) {
		t.Fatalf("pass start event = %+v", got)
	}
	// Задачу прохода отправитель читает из шага, который называет событие.
	if step := stepOf(t, s, 1); step.Goal != startGoal {
		t.Fatalf("step 1 of the pass = %+v, want its goal", step)
	}
	if got := eventsOf(t, s, "v", "request_opened"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Request: req.ID, Sequence: 1, Node: "ask"}}) {
		t.Fatalf("node request event = %+v", got)
	}
	var state string
	var attempts, settled int
	mustNil(t, s.db.QueryRow(`SELECT state, attempts, settled_attempts FROM telegram_outbox LIMIT 1`).Scan(&state, &attempts, &settled))
	if state != "pending" || attempts != 0 || settled != 0 {
		t.Fatalf("a mirrored notification is %s:%d/%d, want queued", state, attempts, settled)
	}
}

// Строка очереди не копирует текст шага: задача прохода (до 4 КиБ) и причина
// отказа шага (до 1 КиБ) живут в шаге и после записи не меняются, а строк —
// по одной на читателя, и копия умножила бы outbox на их число. Отправитель
// читает их из шага (GetSwarmStep).
func TestMirrorRowsCarryNoTextOfTheStep(t *testing.T) {
	ctx := context.Background()
	s := mirrorDispatchStore(t)
	const reason = "delivery failed before the send"
	mustNil(t, s.RefuseActorStep(ctx, attestOne(t, s, "req"), reason))
	rows, err := s.db.Query(`SELECT kind, payload FROM telegram_outbox ORDER BY id`)
	mustNil(t, err)
	defer rows.Close()
	kinds := map[string]int{}
	for rows.Next() {
		var kind, payload string
		mustNil(t, rows.Scan(&kind, &payload))
		kinds[kind]++
		for _, text := range []string{startGoal, reason, `"goal"`, `"detail"`} {
			if strings.Contains(payload, text) {
				t.Fatalf("%s row copies %q: %s", kind, text, payload)
			}
		}
	}
	mustNil(t, rows.Err())
	if kinds["pass_started"] != 3 || kinds["step_failed"] != 3 {
		t.Fatalf("rows by kind = %v, want a pass start and a step failure for each of 3 readers", kinds)
	}
	// Задача — у первого шага прохода, причина — у отказанного шага.
	step, err := s.GetSwarmStep(ctx, "one", 1)
	mustNil(t, err)
	if step.Sequence != 1 || step.NodeID != "build" || step.State != "refused" || step.Goal != startGoal || step.Detail != reason || step.Pass != 1 {
		t.Fatalf("step 1 = %+v", step)
	}
	for _, missing := range []struct {
		swarm    string
		sequence int64
	}{{"one", 2}, {"two", 1}, {"", 1}} {
		if _, err := s.GetSwarmStep(ctx, missing.swarm, missing.sequence); !errors.Is(err, ErrNotFound) {
			t.Fatalf("step %d of swarm %q: %v, want ErrNotFound", missing.sequence, missing.swarm, err)
		}
	}
}

func TestMirrorSkipsUnsubscribedUnlinkedAndDisabledReaders(t *testing.T) {
	ctx := context.Background()
	s := mirrorStore(t)
	// v подписан только на начало прохода, root снял привязку, у d —
	// отключённого администратора — привязка формально жива.
	_, err := s.SetTelegramEvents(ctx, "v", []string{"pass_started"})
	mustNil(t, err)
	_, err = s.UnlinkTelegram(ctx, "root")
	mustNil(t, err)
	seedUser(t, s, "d", "disabled")
	mustNil(t, s.SetUserAdmin(ctx, "d", true))
	linkTelegram(t, s, "d", "", 16)
	_, err = s.db.Exec(`UPDATE users SET disabled = 1 WHERE id = 'd'`)
	mustNil(t, err)
	// Ошибка привязки — не живая привязка.
	seedUser(t, s, "e", "erred")
	mustNil(t, s.AddMember(ctx, "one", "e"))
	mustNil(t, s.SetSwarmMember(ctx, "one", "e", "editor"))
	linkTelegram(t, s, "e", "", 17)
	_, err = s.RecordTelegramLinkError(ctx, "e", "Telegram forbade the chat", "")
	mustNil(t, err)
	mustStart(t, s, "approve")
	req := openRequestOf(t, s, "node")
	sameStrings(t, "queue", queued(t, s), []string{
		"u:pass_started:swarm/one/pass/1", "v:pass_started:swarm/one/pass/1",
		"u:request_opened:swarm/one/request/" + req.ID,
	})
}

func TestMirrorRowsRollBackWithTheDomainChange(t *testing.T) {
	ctx := context.Background()
	s := mirrorStore(t)
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	rollback := errors.New("the caller rolls back")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.StartSwarm(ctx, SwarmStart{SwarmID: "one", UserID: "u", Definition: "approve", Commit: startCommit, Goal: startGoal}, allow); err != nil {
			return err
		}
		if _, err := s.OpenActorRequest(ctx, ActorRequestInput{SwarmID: "one", ActorUID: "a", ActorIncarnation: 1, Kind: "text", Question: "Which db?"}); err != nil {
			return err
		}
		// Строки уже в транзакции изменения: 3 читателя × (начало прохода,
		// запрос узла, вопрос актора).
		var n int
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT count(*) FROM telegram_outbox`).Scan(&n); err != nil {
			return err
		}
		if n != 9 {
			return fmt.Errorf("the transaction holds %d notifications, want 9", n)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("transaction: %v", err)
	}
	sameStrings(t, "queue after the rollback", queued(t, s), []string{})
	if w, err := s.GetSwarm(ctx, "one"); err != nil || w.Execution != "idle" {
		t.Fatalf("swarm after the rollback: %+v %v", w, err)
	}
}

// outboxWriteFails — сбой записи уведомления вида kind: база отказывает
// вставке его строк в outbox, как отказала бы на полном диске.
func outboxWriteFails(kind string) string {
	return `CREATE TRIGGER telegram_outbox_write_fails BEFORE INSERT ON telegram_outbox WHEN NEW.kind = '` + kind + `' BEGIN SELECT RAISE(ABORT, 'telegram outbox write fails'); END`
}

// mirrorReviewStore — mirrorStore с проходом reviewed: оператор ответил на
// первый шаг, шаг 2 актора доставлен; его исход откроет узловой запрос
// approve.
func mirrorReviewStore(t *testing.T) (*Store, ActorStepAttempt) {
	t.Helper()
	s := mirrorStore(t)
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	publish(t, s, "one", map[string]string{"release": startRelease, "approve": startApprove, "build-first": startBuildFirst, "reviewed": reportGraph})
	mustStart(t, s, "reviewed")
	answerNode(t, s, 1, "success")
	return s, deliverOne(t, s, "req-2")
}

// actorAnswerStore — mirrorStore с вопросом актора a; answered — и с ответом
// на него.
func actorAnswerStore(t *testing.T, answered bool) (*Store, SwarmRequest) {
	t.Helper()
	s := mirrorStore(t)
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	req, err := s.OpenActorRequest(context.Background(), ActorRequestInput{SwarmID: "one", ActorUID: "a", ActorIncarnation: 1, Kind: "text", Question: "Which db?"})
	mustNil(t, err)
	if answered {
		mustNil(t, answer(s, req, "u", "", "sqlite"))
	}
	return s, req
}

// Изменение, чьё уведомление не записалось, не фиксируется: SQLite при
// отказе откатывает только сам INSERT, и доменная запись осталась бы без
// уведомления, не верни метод отказ всей транзакции. Сбой бьёт ровно тот вид,
// который ставит проверяемый путь, даже когда тот же вызов прежде ставит
// другой; без сбоя тот же вызов проходит и ставит уведомление этого вида.
func TestFailedNotificationWriteRollsBackTheDomainChange(t *testing.T) {
	ctx := context.Background()
	tables := append([]string{"telegram_outbox"}, executionTables...)
	start := func(definition string) func(t *testing.T) (*Store, func(*Store) error) {
		return func(t *testing.T) (*Store, func(*Store) error) {
			return mirrorStore(t), func(s *Store) error {
				_, err := startOne(s, definition)
				return err
			}
		}
	}
	answerApprove := func(t *testing.T) (*Store, func(*Store) error) {
		s := mirrorStore(t)
		mustStart(t, s, "approve")
		req := openRequestOf(t, s, "node")
		return s, func(s *Store) error { return answer(s, req, "u", "success", "") }
	}
	for _, tc := range []struct {
		name, kind string
		// setup готовит рой без сбоя и возвращает изменение, которое ставит
		// уведомление вида kind.
		setup func(t *testing.T) (*Store, func(s *Store) error)
	}{
		{"pass start", "pass_started", start("approve")},
		{"node request of the first step", "request_opened", start("approve")},
		{"stop", "pass_finished", func(t *testing.T) (*Store, func(*Store) error) {
			s := mirrorStore(t)
			mustStart(t, s, "release")
			return s, func(s *Store) error {
				_, err := s.StopSwarm(ctx, "one", "u", allow)
				return err
			}
		}},
		{"node request answered", "request_answered", answerApprove},
		{"pass finished by an answer", "pass_finished", answerApprove},
		{"node request opened by the graph", "request_opened", func(t *testing.T) (*Store, func(*Store) error) {
			s, a := mirrorReviewStore(t)
			return s, func(s *Store) error { return completeWithReport(s, a, "success", "Implemented the parser.") }
		}},
		{"actor question", "actor_question", func(t *testing.T) (*Store, func(*Store) error) {
			s := mirrorStore(t)
			setActor(t, s, "a", "phase='running', stage='done', operation='start'")
			return s, func(s *Store) error {
				_, err := s.OpenActorRequest(ctx, ActorRequestInput{SwarmID: "one", ActorUID: "a", ActorIncarnation: 1, Kind: "text", Question: "Which db?"})
				return err
			}
		}},
		{"actor question answered", "request_answered", func(t *testing.T) (*Store, func(*Store) error) {
			s, req := actorAnswerStore(t, false)
			return s, func(s *Store) error { return answer(s, req, "u", "", "sqlite") }
		}},
		{"answer delivery changed", "answer_delivery", func(t *testing.T) (*Store, func(*Store) error) {
			s, req := actorAnswerStore(t, true)
			return s, func(s *Store) error {
				_, err := s.RecordSwarmRequestDelivery(ctx, "one", req.ID, "failed")
				return err
			}
		}},
		{"step refused at reservation", "step_failed", func(t *testing.T) (*Store, func(*Store) error) {
			s := mirrorDispatchStore(t)
			_, err := s.db.Exec(`DELETE FROM swarm_role_bindings WHERE swarm_id='one'`)
			mustNil(t, err)
			return s, func(s *Store) error {
				_, err := s.ReserveActorStep(ctx, "one", "req", "swarm:one")
				switch {
				case errors.Is(err, ErrActorStepRefused):
					// Отказ резерва фиксирует refused шага: это его успех.
					return nil
				case err == nil:
					return errors.New("the step was reserved without a binding")
				}
				return err
			}
		}},
		{"step refused after attestation", "step_failed", func(t *testing.T) (*Store, func(*Store) error) {
			s := mirrorDispatchStore(t)
			a := attestOne(t, s, "req")
			return s, func(s *Store) error { return s.RefuseActorStep(ctx, a, "delivery failed before the send") }
		}},
		{"step unconfirmed", "step_failed", func(t *testing.T) (*Store, func(*Store) error) {
			s := mirrorDispatchStore(t)
			a := attestOne(t, s, "req")
			return s, func(s *Store) error { return s.UnconfirmActorStep(ctx, a, "instruction delivery outcome is unknown") }
		}},
		{"step recovered at startup", "step_failed", func(t *testing.T) (*Store, func(*Store) error) {
			s := mirrorDispatchStore(t)
			reserveOne(t, s, "req")
			return s, func(s *Store) error { return s.RecoverActorSteps(ctx) }
		}},
		{"pass finished by an actor", "pass_finished", func(t *testing.T) (*Store, func(*Store) error) {
			s := mirrorDispatchStore(t)
			a := attestOne(t, s, "req")
			mustNil(t, s.FinishActorStepDelivery(ctx, a))
			return s, func(s *Store) error {
				return s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "fail", "")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, change := tc.setup(t)
			queuedBefore := len(queuedOf(t, s, tc.kind))
			before := dumpTables(t, s, tables...)
			_, err := s.db.Exec(outboxWriteFails(tc.kind))
			mustNil(t, err)
			if err := change(s); err == nil || !strings.Contains(err.Error(), "telegram outbox write fails") {
				t.Fatalf("change whose %s notification failed to write: %v, want the write failure", tc.kind, err)
			}
			if after := dumpTables(t, s, tables...); after != before {
				t.Fatalf("a change without its %s notification committed:\n%s\nwant\n%s", tc.kind, after, before)
			}
			_, err = s.db.Exec(`DROP TRIGGER telegram_outbox_write_fails`)
			mustNil(t, err)
			mustNil(t, change(s))
			if n := len(queuedOf(t, s, tc.kind)); n <= queuedBefore {
				t.Fatalf("without the failure the change queued no %s notification", tc.kind)
			}
		})
	}
}

func TestAnswerQueuesTheAnswerBeforeThePassEnd(t *testing.T) {
	ctx := context.Background()
	s := mirrorStore(t)
	mustStart(t, s, "approve")
	req := openRequestOf(t, s, "node")
	before := len(queued(t, s))
	_, err := s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: req.ID, UserID: "u", Outcome: "success"}, allow)
	mustNil(t, err)
	request := "swarm/one/request/" + req.ID
	sameStrings(t, "queue after the answer", queued(t, s)[before:], []string{
		"root:request_answered:" + request, "u:request_answered:" + request, "v:request_answered:" + request,
		"root:pass_finished:swarm/one/pass/1", "u:pass_finished:swarm/one/pass/1", "v:pass_finished:swarm/one/pass/1",
	})
	if got := eventsOf(t, s, "u", "request_answered"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Request: req.ID, Sequence: 1, Node: "ask"}}) {
		t.Fatalf("answer event = %+v", got)
	}
	if got := eventsOf(t, s, "u", "pass_finished"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Sequence: 1, Node: "ask", Execution: "succeeded"}}) {
		t.Fatalf("pass end event = %+v", got)
	}
	// Повтор применённого ответа ничего не пишет, и уведомления тоже.
	after := len(queued(t, s))
	_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: req.ID, UserID: "u", Outcome: "success"}, allow)
	mustNil(t, err)
	if n := len(queued(t, s)); n != after {
		t.Fatalf("a replayed answer queued %d notifications", n-after)
	}
}

func TestStopQueuesThePassStopped(t *testing.T) {
	ctx := context.Background()
	s := mirrorStore(t)
	mustStart(t, s, "release")
	_, err := s.StopSwarm(ctx, "one", "u", allow)
	mustNil(t, err)
	sameStrings(t, "pass ends", queuedOf(t, s, "pass_finished"), readers("swarm/one/pass/1"))
	if got := eventsOf(t, s, "root", "pass_finished"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Sequence: 1, Execution: "stopped"}}) {
		t.Fatalf("stop event = %+v", got)
	}
	// Следующий проход — свой номер.
	mustStart(t, s, "release")
	sameStrings(t, "pass starts", queuedOf(t, s, "pass_started"), append(readers("swarm/one/pass/1"), readers("swarm/one/pass/2")...))
}

// Проход называется своим первым шагом, где бы он ни кончился: остановка на
// втором шаге прохода 1, исход актора на втором шаге прохода, начатого с
// шага 3.
func TestPassIsNamedByItsFirstStepWhereverItEnds(t *testing.T) {
	ctx := context.Background()
	s := mirrorStore(t)
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	mustStart(t, s, "release")
	mustNil(t, answer(s, openRequestOf(t, s, "node"), "u", "success", ""))
	if step := stepOf(t, s, 2); step.NodeID != "build" || step.State != "pending" {
		t.Fatalf("step 2 after Ship = %+v", step)
	}
	_, err := s.StopSwarm(ctx, "one", "u", allow)
	mustNil(t, err)
	mustStart(t, s, "release")
	mustNil(t, answer(s, openRequestOf(t, s, "node"), "u", "success", ""))
	completeBuild(t, s, 4, "req", "success")
	passes := append(readers("swarm/one/pass/1"), readers("swarm/one/pass/3")...)
	sameStrings(t, "pass starts", queuedOf(t, s, "pass_started"), passes)
	sameStrings(t, "pass ends", queuedOf(t, s, "pass_finished"), passes)
	if got := eventsOf(t, s, "v", "pass_started"); !reflect.DeepEqual(got, []TelegramEvent{
		{Swarm: "one", Sequence: 1, Node: "ask", Execution: "waiting"},
		{Swarm: "one", Sequence: 3, Node: "ask", Execution: "waiting"},
	}) {
		t.Fatalf("pass start events = %+v", got)
	}
	if got := eventsOf(t, s, "v", "pass_finished"); !reflect.DeepEqual(got, []TelegramEvent{
		{Swarm: "one", Sequence: 1, Execution: "stopped"},
		{Swarm: "one", Sequence: 3, Node: "build", Execution: "succeeded"},
	}) {
		t.Fatalf("pass end events = %+v", got)
	}
}

func TestActorQuestionAnswerAndDeliveryChangesAreMirrored(t *testing.T) {
	ctx := context.Background()
	s := mirrorStore(t)
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	req, err := s.OpenActorRequest(ctx, ActorRequestInput{SwarmID: "one", ActorUID: "a", ActorIncarnation: 1, Kind: "text", Question: "Which db?"})
	mustNil(t, err)
	request := "swarm/one/request/" + req.ID
	sameStrings(t, "questions", queuedOf(t, s, "actor_question"), readers(request))
	if got := eventsOf(t, s, "u", "actor_question"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Request: req.ID}}) {
		t.Fatalf("actor question event = %+v", got)
	}
	_, err = s.AnswerSwarmRequest(ctx, SwarmAnswer{SwarmID: "one", RequestID: req.ID, UserID: "u", Response: "sqlite"}, allow)
	mustNil(t, err)
	sameStrings(t, "answers", queuedOf(t, s, "request_answered"), readers(request))
	// Исход доставки пишется уведомлением, только когда он меняется:
	// повтор failed и failed поверх unconfirmed (остаётся unconfirmed) —
	// без уведомления.
	for _, outcome := range []string{"failed", "failed", "unconfirmed", "failed", "delivered"} {
		_, err := s.RecordSwarmRequestDelivery(ctx, "one", req.ID, outcome)
		mustNil(t, err)
	}
	var deliveries []string
	for _, ev := range eventsOf(t, s, "v", "answer_delivery") {
		if ev.Swarm != "one" || ev.Request != req.ID {
			t.Fatalf("delivery event of another object: %+v", ev)
		}
		deliveries = append(deliveries, ev.Delivery)
	}
	sameStrings(t, "delivery outcomes", deliveries, []string{"failed", "unconfirmed", "delivered"})
	if n := len(queuedOf(t, s, "answer_delivery")); n != 9 {
		t.Fatalf("%d delivery notifications, want 3 for each of 3 readers", n)
	}
}

func TestStepFailuresAreMirrored(t *testing.T) {
	ctx := context.Background()
	t.Run("refused at reservation", func(t *testing.T) {
		s := mirrorDispatchStore(t)
		_, err := s.db.Exec(`DELETE FROM swarm_role_bindings WHERE swarm_id='one'`)
		mustNil(t, err)
		if _, err := s.ReserveActorStep(ctx, "one", "req", "swarm:one"); !errors.Is(err, ErrActorStepRefused) {
			t.Fatalf("reserve without a binding: %v", err)
		}
		sameStrings(t, "step failures", queuedOf(t, s, "step_failed"), readers("swarm/one/step/1"))
		if got := eventsOf(t, s, "u", "step_failed"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Sequence: 1, Node: "build", State: "refused"}}) {
			t.Fatalf("refused step event = %+v", got)
		}
		// Причину отправитель читает из шага, который называет событие.
		if step := stepOf(t, s, 1); step.State != "refused" || step.Detail == "" {
			t.Fatalf("refused step = %+v, want its reason", step)
		}
	})
	t.Run("refused after attestation", func(t *testing.T) {
		s := mirrorDispatchStore(t)
		a := attestOne(t, s, "req")
		mustNil(t, s.RefuseActorStep(ctx, a, "delivery failed before the send"))
		if got := eventsOf(t, s, "v", "step_failed"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Sequence: 1, Node: "build", State: "refused"}}) {
			t.Fatalf("refused step event = %+v", got)
		}
		if step := stepOf(t, s, 1); step.Detail != "delivery failed before the send" {
			t.Fatalf("refused step = %+v, want its reason", step)
		}
	})
	t.Run("unconfirmed", func(t *testing.T) {
		s := mirrorDispatchStore(t)
		a := attestOne(t, s, "req")
		mustNil(t, s.UnconfirmActorStep(ctx, a, "instruction delivery outcome is unknown"))
		if got := eventsOf(t, s, "root", "step_failed"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Sequence: 1, Node: "build", State: "unconfirmed"}}) {
			t.Fatalf("unconfirmed step event = %+v", got)
		}
		if step := stepOf(t, s, 1); step.Detail != "instruction delivery outcome is unknown" {
			t.Fatalf("unconfirmed step = %+v, want its reason", step)
		}
	})
	t.Run("delivered", func(t *testing.T) {
		s := mirrorDispatchStore(t)
		a := attestOne(t, s, "req")
		mustNil(t, s.FinishActorStepDelivery(ctx, a))
		sameStrings(t, "step failures", queuedOf(t, s, "step_failed"), []string{})
	})
	t.Run("recovered at startup", func(t *testing.T) {
		s := mirrorDispatchStore(t)
		reserveOne(t, s, "req")
		mustNil(t, s.RecoverActorSteps(ctx))
		if got := eventsOf(t, s, "u", "step_failed"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Sequence: 1, Node: "build", State: "unconfirmed"}}) {
			t.Fatalf("recovered step event = %+v", got)
		}
		if step := stepOf(t, s, 1); step.Detail != detailRestartAttestation {
			t.Fatalf("recovered step = %+v, want its reason", step)
		}
		// Повтор восстановления шагов не находит и ничего не пишет.
		before := len(queued(t, s))
		mustNil(t, s.RecoverActorSteps(ctx))
		if n := len(queued(t, s)); n != before {
			t.Fatalf("a repeated recovery queued %d notifications", n-before)
		}
	})
	t.Run("stopped delivery is the pass end", func(t *testing.T) {
		s := mirrorDispatchStore(t)
		attestOne(t, s, "req")
		_, err := s.StopSwarm(ctx, "one", "u", allow)
		mustNil(t, err)
		sameStrings(t, "step failures", queuedOf(t, s, "step_failed"), []string{})
		sameStrings(t, "pass ends", queuedOf(t, s, "pass_finished"), readers("swarm/one/pass/1"))
	})
}

func TestFailedActorStepFinishesThePassFailed(t *testing.T) {
	ctx := context.Background()
	s := mirrorDispatchStore(t)
	a := attestOne(t, s, "req")
	mustNil(t, s.FinishActorStepDelivery(ctx, a))
	mustNil(t, s.CompletePipelineActorVisit(ctx, "one", "one", 1, "req", "a", 1, "fail", ""))
	if got := eventsOf(t, s, "v", "pass_finished"); !reflect.DeepEqual(got, []TelegramEvent{{Swarm: "one", Sequence: 1, Node: "build", Execution: "failed"}}) {
		t.Fatalf("failed pass event = %+v", got)
	}
	sameStrings(t, "step failures", queuedOf(t, s, "step_failed"), []string{})
}

// mirrorDispatchStore — mirrorStore с проходом build-first: первый шаг build
// ждёт доставки, роль build привязана к a.
func mirrorDispatchStore(t *testing.T) *Store {
	t.Helper()
	s := mirrorStore(t)
	setActor(t, s, "a", "phase='running', stage='done', operation='start'")
	if _, err := startOne(s, "build-first"); err != nil {
		t.Fatal(err)
	}
	return s
}
