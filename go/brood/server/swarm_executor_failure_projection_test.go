package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// FEATURE: Registered executor failure is a distinct read-only public fact (#292).
// SPEC: Exact keyed reason/time, one settled slot, unchanged task/report/transport;
// human trace/history/meta and scoped guest status/join/job agree without effects.
// IMPLEMENTATION: Explicit bounded DTOs; guest failure reads share the existing authority snapshot.
// RESULT: IMPLEMENTED — public API, download, guest and isolation regressions pass.
// STATUS: DONE
func TestRegisteredExecutorFailurePublicProjection(t *testing.T) {
	d, intent, child := ephemeralChildServerFixture(t)
	ctx := context.Background()
	reason := "Executor rejected locally.\nNo actor result or cleanup authority."
	if err := d.srv.store.RecordEphemeralChildExecutorFailure(ctx, intent.ID, reason, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	fact, err := d.srv.store.GetEphemeralChildExecutorFailure(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{"intentId": intent.ID, "childId": child.ID, "requestId": child.RequestID, "reason": reason, "failedAt": fact.FailedAt}
	before, messages := d.calls()
	path := "/api/swarms/" + d.swarm
	response, body, _ := d.do(d.alice, "GET", path+"/passes/1", "", nil)
	expectStatus(t, "registered failure trace", response, body, http.StatusOK)
	step := body["steps"].([]any)[0].(map[string]any)
	job := traceEphemeralJob(t, body)
	if !reflect.DeepEqual(job["executorFailure"], expected) || job["failure"] != nil || job["resultRetention"] != nil {
		t.Fatalf("distinct executor fact missing: %+v", job)
	}
	group := step["children"].(map[string]any)
	if group["executorFailed"] != float64(1) || group["registered"] != float64(1) || group["unregisteredFailed"] != float64(0) || group["open"] != float64(1) {
		t.Fatalf("wrong slot accounting: %+v", group)
	}
	task := group["children"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(task["executorFailure"], expected) || task["state"] != child.State || task["delivery"] != child.Delivery || task["report"] != nil || task["outcome"] != nil || task["finishedAt"] != nil {
		t.Fatalf("failure became actor result: %+v", task)
	}
	response, history, _ := d.do(d.alice, "GET", path+"/children/1", "", nil)
	expectStatus(t, "registered failure history", response, history, http.StatusOK)
	if !reflect.DeepEqual(history, group) {
		t.Fatal("trace/history disagree")
	}
	response, page, _ := d.do(d.alice, "GET", path+"/children", "", nil)
	expectStatus(t, "registered failure metadata", response, page, http.StatusOK)
	if page["items"].([]any)[0].(map[string]any)["executorFailed"] != float64(1) {
		t.Fatal("metadata lost count")
	}
	response, download, _ := d.do(d.alice, "GET", path+"/passes/1?download=1", "", nil)
	expectStatus(t, "download", response, download, http.StatusOK)
	if !reflect.DeepEqual(body, download) {
		t.Fatal("download differs")
	}
	parent, peer, _ := childRelay(t, d)
	for _, name := range []string{"child_get", "child_job", "child_join"} {
		args := map[string]any{"child_id": child.ID}
		if name == "child_job" {
			args = map[string]any{"job_id": intent.ID}
		}
		if name == "child_join" {
			args = map[string]any{"parent_sequence": intent.Parent.Sequence, "parent_request_id": intent.Parent.RequestID}
		}
		result := childCall(t, parent, name, args)
		if !result.OK {
			t.Fatalf("guest %s refused: %+v", name, result)
		}
		var data map[string]any
		if err := json.Unmarshal(result.Data, &data); err != nil {
			t.Fatal(err)
		}
		projected := data
		if name == "child_join" {
			if data["executor_failed"] != float64(1) || data["open"] != float64(1) || data["joined"] != false {
				t.Fatalf("join accounting: %+v", data)
			}
			projected = data["children"].([]any)[0].(map[string]any)
		}
		guestFact := map[string]any{"intent_id": intent.ID, "child_id": child.ID, "request_id": child.RequestID, "reason": reason, "failed_at": fact.FailedAt}
		if !reflect.DeepEqual(projected["executor_failure"], guestFact) || projected["failure"] != nil || projected["report"] != nil || projected["outcome"] != nil {
			t.Fatalf("guest %s lost distinct fact: %+v", name, data)
		}
		if denied := childCall(t, peer, name, args); denied.OK || len(denied.Data) != 0 {
			t.Fatalf("peer accessed %s: %+v", name, denied)
		}
	}
	bob := newClient(t)
	d.login(bob, "bob", "bob-password-1")
	response, hidden, _ := d.do(bob, "GET", path+"/passes/1", "", nil)
	expectStatus(t, "hidden executor failure", response, hidden, http.StatusNotFound)
	after, sent := d.calls()
	if len(before) != len(after) || len(messages) != len(sent) {
		t.Fatal("projection triggered effects")
	}
	retained, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil || retained.Task != child {
		t.Fatalf("read changed task: %+v %v", retained, err)
	}
}
