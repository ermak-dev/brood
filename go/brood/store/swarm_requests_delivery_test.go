package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// FEATURE: Состояние доставки ответа актору (#210).
// SPEC: ответ на запрос актора фиксируется с доставкой pending; исход
// доставки пишется отдельно и после коммита ответа: сбой не откатывает
// ответ, повтор не продвигает граф, доставленный ответ не переписывается.
// У узлового запроса адресата нет, доставки тоже.
// IMPLEMENTATION: колонка delivery, RecordSwarmRequestDelivery, CHECK и
// ограждение неизменности в схеме.
// RESULT: IMPLEMENTED
// STATUS: DONE

func TestActorAnswerIsCommittedWithPendingDelivery(t *testing.T) {
	s, node := requestStore(t)
	req := actorRequest(t, s, "text", nil)
	if req.Delivery != "" {
		t.Fatalf("open request has delivery %q", req.Delivery)
	}
	mustNil(t, answer(s, req, "u", "", "use sqlite"))
	got, err := s.GetSwarmRequest(context.Background(), "s", req.ID)
	if err != nil || got.State != "answered" || got.Delivery != "pending" {
		t.Fatalf("answered actor request: %+v %v", got, err)
	}
	mustNil(t, answer(s, node, "u", "success", "ship"))
	stored, err := s.GetSwarmRequest(context.Background(), "s", node.ID)
	if err != nil || stored.Delivery != "" {
		t.Fatalf("node request has a recipient: %+v %v", stored, err)
	}
}

func TestFailedDeliveryKeepsTheAnswerAndRetryDoesNotMoveTheGraph(t *testing.T) {
	ctx := context.Background()
	s, node := requestStore(t)
	mustNil(t, answer(s, node, "u", "success", "ship"))
	steps := stepStates(t, s)
	req := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, req, "u", "", "use sqlite"))
	answered, err := s.GetSwarmRequest(ctx, "s", req.ID)
	mustNil(t, err)

	failed, err := s.RecordSwarmRequestDelivery(ctx, "s", req.ID, "failed")
	if err != nil || failed.Delivery != "failed" {
		t.Fatalf("failed delivery: %+v %v", failed, err)
	}
	answered.Delivery = "failed"
	if !reflect.DeepEqual(failed, answered) {
		t.Fatalf("failed delivery changed the answer:\n got %+v\nwant %+v", failed, answered)
	}
	failed, err = s.RecordSwarmRequestDelivery(ctx, "s", req.ID, "failed")
	if err != nil || failed.Delivery != "failed" {
		t.Fatalf("second failure: %+v %v", failed, err)
	}
	delivered, err := s.RecordSwarmRequestDelivery(ctx, "s", req.ID, "delivered")
	if err != nil || delivered.Delivery != "delivered" || delivered.State != "answered" || *delivered.Response != "use sqlite" {
		t.Fatalf("retried delivery: %+v %v", delivered, err)
	}
	for _, outcome := range []string{"delivered", "failed", "unconfirmed"} {
		if _, err := s.RecordSwarmRequestDelivery(ctx, "s", req.ID, outcome); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("delivered answer re-recorded (%s): %v", outcome, err)
		}
	}
	if got := stepStates(t, s); !reflect.DeepEqual(got, steps) {
		t.Fatalf("delivery moved the graph: %v, want %v", got, steps)
	}
	if got, _ := s.GetSwarm(ctx, "s"); got.Execution != "running" {
		t.Fatalf("execution %q", got.Execution)
	}
}

func TestDeliveryIsOnlyForAnsweredActorRequestsOfTheSwarm(t *testing.T) {
	ctx := context.Background()
	s, node := requestStore(t)
	open := actorRequest(t, s, "text", nil)
	cancelled := actorRequest(t, s, "text", nil)
	mustNil(t, s.CancelSwarmRequest(ctx, "s", cancelled.ID, "u", allow))
	answered := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, answered, "u", "", "yes"))
	for name, id := range map[string]string{"open node": node.ID, "open actor": open.ID, "cancelled": cancelled.ID} {
		if _, err := s.RecordSwarmRequestDelivery(ctx, "s", id, "delivered"); !errors.Is(err, ErrPipelineVisitConflict) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	mustNil(t, answer(s, node, "u", "success", "ship"))
	steps := stepStates(t, s)
	if _, err := s.RecordSwarmRequestDelivery(ctx, "s", node.ID, "delivered"); !errors.Is(err, ErrPipelineVisitConflict) {
		t.Fatalf("answered node request: %v", err)
	}
	if got := stepStates(t, s); !reflect.DeepEqual(got, steps) {
		t.Fatalf("node delivery moved the graph: %v", got)
	}
	// Запрос чужого роя по его ID не находится.
	if _, err := s.db.Exec(`INSERT INTO swarms(id,project_id,owner_id,name,atespace_name,state,created_at,updated_at) VALUES ('other','p','u','other','other','active','now','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSwarmRequestDelivery(ctx, "other", answered.ID, "delivered"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign swarm: %v", err)
	}
	if got, _ := s.GetSwarmRequest(ctx, "s", answered.ID); got.Delivery != "pending" {
		t.Fatalf("foreign delivery wrote: %+v", got)
	}
}

func TestSchemaKeepsDeliveryApartFromTheAnswer(t *testing.T) {
	ctx := context.Background()
	s, node := requestStore(t)
	open := actorRequest(t, s, "text", nil)
	req := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, req, "u", "", "yes"))
	mustNil(t, answer(s, node, "u", "success", ""))
	for name, stmt := range map[string]string{
		"delivery on open":        `UPDATE swarm_requests SET delivery='pending' WHERE id='` + open.ID + `'`,
		"delivery on node":        `UPDATE swarm_requests SET delivery='delivered' WHERE id='` + node.ID + `'`,
		"answer without delivery": `UPDATE swarm_requests SET state='answered',answered_at='now',answered_by='u',response='x' WHERE id='` + open.ID + `'`,
		"rewrite with delivery":   `UPDATE swarm_requests SET delivery='delivered',response='rewrite' WHERE id='` + req.ID + `'`,
		"unknown state":           `UPDATE swarm_requests SET delivery='sent' WHERE id='` + req.ID + `'`,
		"back to pending":         `UPDATE swarm_requests SET delivery='failed' WHERE id='` + req.ID + `'; UPDATE swarm_requests SET delivery='pending' WHERE id='` + req.ID + `'`,
		"unconfirmed to pending":  `UPDATE swarm_requests SET delivery='unconfirmed' WHERE id='` + req.ID + `'; UPDATE swarm_requests SET delivery='pending' WHERE id='` + req.ID + `'`,
		"unconfirmed to failed":   `UPDATE swarm_requests SET delivery='unconfirmed' WHERE id='` + req.ID + `'; UPDATE swarm_requests SET delivery='failed' WHERE id='` + req.ID + `'`,
	} {
		before := snapshotRequests(t, s)
		tx, err := s.db.BeginTx(ctx, nil)
		mustNil(t, err)
		_, err = tx.ExecContext(ctx, stmt)
		mustNil(t, tx.Rollback())
		if err == nil {
			t.Errorf("%s accepted by the schema", name)
		}
		if after := snapshotRequests(t, s); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: rollback left changes", name)
		}
	}
	for _, state := range []string{"failed", "unconfirmed", "unconfirmed", "delivered"} {
		if _, err := s.db.Exec(`UPDATE swarm_requests SET delivery=? WHERE id=?`, state, req.ID); err != nil {
			t.Fatalf("delivery update to %s refused: %v", state, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE swarm_requests SET delivery='failed' WHERE id=?`, req.ID); err == nil {
		t.Fatal("delivered answer was reopened")
	}
}

// FEATURE: Неизвестный исход доставки ответа (#260).
// SPEC: ответ ушёл в сессию актора, а квитанции нет — доставка unconfirmed,
// не failed: ответ мог дойти. Повтор из unconfirmed разрешён и может
// доставить ответ дважды. Повтор, заведомо не отправленный, неизвестности
// не снимает: unconfirmed остаётся, failed поверх него не пишется. Исход —
// только delivered, failed или unconfirmed.
// IMPLEMENTATION: RecordSwarmRequestDelivery с исходом попытки, CHECK и
// swarm_requests_fence.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestUnconfirmedDeliveryStaysUnknownUntilDelivered(t *testing.T) {
	ctx := context.Background()
	s, _ := requestStore(t)
	req := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, req, "u", "", "use sqlite"))
	answered, err := s.GetSwarmRequest(ctx, "s", req.ID)
	mustNil(t, err)
	for _, step := range []struct{ outcome, want string }{
		{"unconfirmed", "unconfirmed"},
		{"failed", "unconfirmed"},
		{"unconfirmed", "unconfirmed"},
		{"delivered", "delivered"},
	} {
		got, err := s.RecordSwarmRequestDelivery(ctx, "s", req.ID, step.outcome)
		if err != nil || got.Delivery != step.want {
			t.Fatalf("%s attempt: %+v %v, want %s", step.outcome, got, err, step.want)
		}
		answered.Delivery = step.want
		if !reflect.DeepEqual(got, answered) {
			t.Fatalf("%s attempt changed the answer:\n got %+v\nwant %+v", step.outcome, got, answered)
		}
	}
	retried := actorRequest(t, s, "text", nil)
	mustNil(t, answer(s, retried, "u", "", "yes"))
	for _, outcome := range []string{"failed", "unconfirmed"} {
		if got, err := s.RecordSwarmRequestDelivery(ctx, "s", retried.ID, outcome); err != nil || got.Delivery != outcome {
			t.Fatalf("%s after failed: %+v %v", outcome, got, err)
		}
	}
	for _, outcome := range []string{"", "pending", "sent", "Delivered"} {
		if _, err := s.RecordSwarmRequestDelivery(ctx, "s", retried.ID, outcome); !errors.Is(err, ErrSwarmRequestInvalid) {
			t.Fatalf("outcome %q: %v", outcome, err)
		}
	}
	if got, _ := s.GetSwarmRequest(ctx, "s", retried.ID); got.Delivery != "unconfirmed" {
		t.Fatalf("invalid outcome wrote: %+v", got)
	}
}
