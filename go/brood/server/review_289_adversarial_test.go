package server

// BUG: Гостевой mode=any подменяет обязательный default-all actor-узла.
// HYPOTHESIS: Policy берётся из child_create, а не из определения узла.
// SOLUTION: Сервер сохраняет policy снимка узла; guest mode/quorum её не меняют.
// RESULT: FIXED — один отказ даёт fail default-all; bounded human material сохранён.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

// Проверяет контракт #289: отсутствие policy в узле означает all, а не выбор гостя.
func TestReview289DefaultNodeJoinPolicyCannotBeOverridden(t *testing.T) {
	d := newChildEngineServer(t)
	parent, worker, _ := childRelay(t, d)
	firstArgs := childGuestCreate(d, "default-policy-one")
	secondArgs := childGuestCreate(d, "default-policy-two")
	firstArgs["mode"] = "any"
	secondArgs["mode"] = "any"
	first := childResponseID(t, childCall(t, parent, "child_create", firstArgs))
	second := childResponseID(t, childCall(t, parent, "child_create", secondArgs))
	d.srv.children.dispatch(context.Background())
	if r := childCall(t, worker, "child_complete", map[string]any{"child_id": first, "request_id": "default-policy-one", "outcome": "success", "report": "First child succeeded"}); !r.OK {
		t.Fatal(r.Text)
	}
	d.srv.children.dispatch(context.Background())
	if r := childCall(t, worker, "child_complete", map[string]any{"child_id": second, "request_id": "default-policy-two", "outcome": "fail", "report": "Second child failed"}); !r.OK {
		t.Fatal(r.Text)
	}
	r := childCall(t, parent, "child_join", map[string]any{"parent_sequence": 1, "parent_request_id": firstArgs["parent_request_id"], "wait_seconds": 0})
	if !r.OK || r.Outcome != "completed" {
		t.Fatalf("join did not complete: %+v", r)
	}
	step, err := d.srv.store.GetSwarmStep(context.Background(), d.swarm, 1)
	if err != nil {
		t.Fatal(err)
	}
	outcome := "<nil>"
	if step.Outcome != nil {
		outcome = *step.Outcome
	}
	t.Logf("unmodified graph=%s; stored outcome=%s; aggregate=%s", dispatchGraph, outcome, step.Report)
	if outcome != "fail" {
		t.Fatalf("node without an explicit join policy must use all: guest-selected any produced outcome=%s and report=%q", outcome, step.Report)
	}
}

// Проверяет настоящий relay -> join -> HTTP material без копии агрегации в тесте.
func TestReview289AggregateReportReachesHumanMaterial(t *testing.T) {
	d := newDispatchServer(t)
	d.srv.children.close()
	d.srv.children = newChildDispatcher(d.srv)
	d.bind("build", d.master.UID)
	d.bind("review", d.worker.UID)
	d.start("reviewed")
	parent, worker, _ := childRelay(t, d)
	args := childGuestCreate(d, "aggregate-one")
	first := childResponseID(t, childCall(t, parent, "child_create", args))
	second := childResponseID(t, childCall(t, parent, "child_create", childGuestCreate(d, "aggregate-two")))
	for _, item := range []struct{ id, key, report string }{
		{first, "aggregate-one", strings.Repeat("a", store.MaxStepReportBytes)},
		{second, "aggregate-two", strings.Repeat("ж", store.MaxStepReportBytes/2)},
	} {
		d.srv.children.dispatch(context.Background())
		if r := childCall(t, worker, "child_complete", map[string]any{"child_id": item.id, "request_id": item.key, "outcome": "success", "report": item.report}); !r.OK {
			t.Fatal(r.Text)
		}
	}
	if r := childCall(t, parent, "child_join", map[string]any{"parent_sequence": 1, "parent_request_id": args["parent_request_id"], "wait_seconds": 0}); !r.OK || r.Outcome != "completed" {
		t.Fatalf("join: %+v", r)
	}
	step, err := d.srv.store.GetSwarmStep(context.Background(), d.swarm, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(step.Report) > store.MaxStepReportBytes || !strings.Contains(step.Report, first) || !strings.Contains(step.Report, second) || !strings.Contains(step.Report, "truncated") {
		t.Fatalf("invalid aggregate length=%d report=%q", len(step.Report), step.Report)
	}
	response, body, _ := d.do(d.alice, "GET", "/api/swarms/"+d.swarm+"/requests", "", nil)
	expectStatus(t, "human requests", response, body, http.StatusOK)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), first) || !strings.Contains(string(raw), second) || !strings.Contains(string(raw), "Child join all: ready_success") {
		t.Fatalf("human material omitted aggregate: %s", raw)
	}
	t.Logf("bounded aggregate=%d bytes; both child IDs/outcomes and truncation markers reach human HTTP material", len(step.Report))
}
