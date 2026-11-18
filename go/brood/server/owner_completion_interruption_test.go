package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

// BUG: Прерванный обычный шаг скрывал причину отказа за общей ошибкой сервиса.
// SPEC: Аутентифицированный гость узнаёт о вмешательстве; граф не продвигается.
func TestOwnerInterruptedCompletionExplainsReconciliation(t *testing.T) {
	d := newChildEngineServer(t)
	parent, _, _ := childRelayWithSession(t, d, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		return relay.SessionResponse{OK: true, Outcome: "completed", Text: "owner interrupt applied"}
	})
	request := d.committed().Request
	r, b, _ := d.do(d.alice, "POST", "/api/commands/session_terminal", d.csrf(d.alice), map[string]any{"swarm_id": d.swarm, "arguments": map[string]any{"name": d.master.Name, "action": "interrupt", "tid": "owner"}})
	expectStatus(t, "owner interruption", r, b, http.StatusOK)
	response := childCall(t, parent, "pipeline_visit_complete", map[string]any{"run_id": d.swarm, "sequence": 1, "request_id": request, "outcome": "success", "report": "late evidence"})
	if response.OK || response.Outcome != "refused" || !strings.Contains(response.Text, "interrupted") || !strings.Contains(response.Text, "reconciliation") {
		t.Fatalf("interruption reason not exposed: %+v", response)
	}
	if d.committed().State != "unconfirmed" {
		t.Fatalf("interrupted step progressed: %+v", d.committed())
	}
}
