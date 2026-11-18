package server

// BUG: Generic pipeline completion выдаёт failed вместо child_join refusal.
// HYPOTHESIS: Адаптер не распознаёт типизированный ErrSwarmChildConflict.
// SOLUTION: Типизированный конфликт возвращает публичный отказ с child_join.
// RESULT: FIXED — отказ не раскрывает внутреннюю ошибку и не закрывает родителя.
// STATUS: FIXED

import (
	"context"
	"strings"
	"testing"
)

func TestChildGenericCompletionReportsJoinConflict(t *testing.T) {
	ctx := context.Background()
	d := newChildEngineServer(t)
	if _, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "one")); err != nil {
		t.Fatal(err)
	}
	v := d.last()
	r := d.srv.managed.handleActorCommand(ctx, d.master, coordinationInvocation("pipeline_visit_complete", map[string]any{"run_id": d.swarm, "sequence": v.Sequence, "request_id": v.RequestID, "outcome": "success"}))
	if r.OK || r.Outcome != "refused" || !strings.Contains(r.Text, "child_join") || strings.Contains(r.Text, "swarm child conflict") || strings.Contains(r.Text, "unavailable") {
		t.Fatalf("expected explicit public child_join refusal, got %+v", r)
	}
	after := d.last()
	if after.State != v.State || after.Outcome != nil || after.Report != "" {
		t.Fatalf("generic completion changed gated parent: %+v", after)
	}
}
