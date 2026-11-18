package store

import (
	"context"
	"reflect"
	"testing"
)

// FEATURE: История ephemeral job у шага до allocation и регистрации child (#292).
// SPEC: Очередь остаётся видимой при полном пуле; чтение не выделяет ресурсов,
// не подтверждает source/result и не восстанавливает отсутствующий inventory.
// IMPLEMENTATION: Bounded FIFO snapshot связывает только реально сохранённые факты.
// RESULT: IMPLEMENTED — исходный RED терял queued job до регистрации child.
// STATUS: DONE
func TestPassTraceRetainsQueuedEphemeralJobBeforeAllocation(t *testing.T) {
	ctx := context.Background()
	s, intent := allocationFixture(t, 1)
	before := databaseRows(t, s.db)
	trace, err := s.GetSwarmPassTrace(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Pass)
	mustNil(t, err)
	if len(trace.Steps) != 1 {
		t.Fatal("fixture pass lost its parent step")
	}
	jobs := trace.Steps[0].EphemeralJobs
	if len(jobs) != 1 || jobs[0].Intent != intent || jobs[0].Allocation != nil || jobs[0].Attestation != nil || jobs[0].ChildID != "" || jobs[0].Inventory != nil || jobs[0].ResultSeal != nil {
		t.Fatalf("queued ephemeral job disappeared or acquired invented facts: %+v", jobs)
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("reading queued job history allocated resources or changed execution")
	}
}
