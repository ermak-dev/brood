package server

// FEATURE: Отчёт актора о шаге доходит до оператора (#233).
// SPEC: pipeline_visit_complete несёт необязательный report. Сервер
// принимает его только вместе с исходом от привязанного к роли актора той
// же инкарнации, хранит на шаге один раз и показывает в виде роя (report
// узла прогона). Узловой запрос, открытый следом, несёт отчёт в context с
// номером шага, и инбокс получает его через API запросов роя. Неверный
// отчёт отвергается гостю с причиной, без записи исхода.
// IMPLEMENTATION: command_guest.go, pipeline_api.go, store
// CompletePipelineActorVisit; гость — подделки dispatchServer.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// dispatchReviewed — актор роли build предлагает решение, оператор его
// одобряет.
const dispatchReviewed = `apiVersion: brood.dev/v1alpha1
name: reviewed
start: build
exit: done
nodes:
  build: {type: actor, role: build, prompt: "Propose the architecture."}
  approve:
    type: human
    question: Approve the proposed architecture?
    options:
      - {label: Approve, outcome: success}
  done: {type: exit}
edges:
  - {from: build, to: approve}
  - {from: approve, to: done}
goalGates: [done]
`

func (d *dispatchServer) completeReport(actor store.ManagedActor, v store.PipelineVisit, outcome, report string) relay.SwarmResponse {
	d.t.Helper()
	inv := coordinationInvocation("pipeline_visit_complete", map[string]any{"run_id": d.swarm, "sequence": v.Sequence, "request_id": v.RequestID, "outcome": outcome, "report": report})
	got := d.srv.managed.handleActorCommand(context.Background(), actor, inv)
	d.srv.steps.wait()
	return got
}

// TestStepReportReachesOperator — отчёт привязанного актора хранится на шаге
// и виден в виде роя; узловой запрос следом несёт его в context с номером
// шага. Чужой актор и неверный текст отвергаются без записи, повтор с
// другим отчётом — конфликт.
func TestStepReportReachesOperator(t *testing.T) {
	d := newDispatchServer(t)
	d.start("reviewed")
	step := d.last()
	if step.NodeID != "build" || step.State != "inflight" {
		t.Fatalf("delivered step: %+v", step)
	}
	const report = "Proposed a layered design.\nBranch feat/9-arch, merge request !9, decision in docs/ARCH.md."
	if got := d.completeReport(d.master, step, "success", report); got.OK || got.Text != "pipeline completion conflict" {
		t.Fatalf("report from an unbound actor: %+v", got)
	}
	if got := d.completeReport(d.worker, step, "success", "Done.\x1b[2J"); got.OK || got.Outcome != "refused" || !strings.Contains(got.Text, "report") {
		t.Fatalf("report with a control character: %+v", got)
	}
	if v := d.last(); v.Sequence != step.Sequence || v.State != "inflight" || v.Report != "" {
		t.Fatalf("refused report changed the step: %+v", v)
	}
	if got := d.completeReport(d.worker, step, "success", report); !got.OK {
		t.Fatalf("completion with a report: %+v", got)
	}
	if got := d.completeReport(d.worker, step, "success", "Another story."); got.OK || got.Text != "pipeline completion conflict" {
		t.Fatalf("retry with another report: %+v", got)
	}
	if got := d.completeReport(d.worker, step, "success", report); !got.OK {
		t.Fatalf("exact retry: %+v", got)
	}

	r, b, _ := d.do(d.alice, "GET", "/api/pipeline-runs/"+d.swarm, "", nil)
	expectStatus(t, "run view", r, b, http.StatusOK)
	nodes := b["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("run nodes: %v", nodes)
	}
	if built := nodes[0].(map[string]any); built["report"] != report || built["status"] != "done" {
		t.Fatalf("reported step on the wire: %v", built)
	}
	if gate := nodes[1].(map[string]any); gate["id"] != "approve" || gate["report"] != nil {
		t.Fatalf("gate step on the wire: %v", gate)
	}

	r, b, _ = d.do(d.alice, "GET", "/api/swarms/"+d.swarm+"/requests", "", nil)
	expectStatus(t, "requests", r, b, http.StatusOK)
	results := b["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("requests: %v", results)
	}
	req := results[0].(map[string]any)
	material, _ := req["context"].(string)
	if req["nodeId"] != "approve" || req["sequence"] != float64(2) || !strings.Contains(material, "step 1") || !strings.Contains(material, report) {
		t.Fatalf("gate request on the wire: %v", req)
	}
}
