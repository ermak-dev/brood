package store

import (
	"context"
	"testing"
)

// FEATURE: История и краткие счётчики не скрывают pre-registration отказ.
// SPEC: failure отдельный факт без child/proof, Open не считает его отсутствующим.
func TestEphemeralFailureHistoryAndSummary(t *testing.T) {
	ctx := context.Background()
	s, job := allocationFixture(t, 1)
	mustNil(t, recordUnregisteredFailure(s, ctx, job.ID, "Historical local decision", childAllow))
	history, err := s.GetEphemeralChildHistory(ctx, job.ID)
	mustNil(t, err)
	if history.Failure == nil {
		t.Fatal("history lost distinct failure fact")
	}
	if history.ChildID != "" || history.Allocation != nil || history.Attestation != nil || history.ResultSeal != nil {
		t.Fatal("queued failure history invented executor evidence")
	}
	groups, err := s.SwarmChildGroups(ctx, job.Parent.Principal.SwarmID, 0, 10)
	mustNil(t, err)
	if len(groups.Items) != 1 || groups.Items[0].Registered != 0 || groups.Items[0].Open != 0 || groups.Items[0].Started != 0 {
		t.Fatalf("summary counted terminal failure as missing child: %+v", groups)
	}
}
