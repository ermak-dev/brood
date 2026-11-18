package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// FEATURE: History retains registered executor decisions independently of inventory.
// SPEC: Single/group projections are read-only, preserve task bytes and never seal results.
// IMPLEMENTATION: Separate historical ExecutorFailure, independent of current inventory.
// RESULT: IMPLEMENTED — exact projection and unchanged database verified.
// STATUS: DONE
func TestExecutorFailureHistoryProjection(t *testing.T) {
	s, job, child := registeredFailureFixture(t)
	ctx := context.Background()
	mustNil(t, s.RecordEphemeralChildExecutorFailure(ctx, job.ID, "Explicit server rejection", childAllow))
	fact, err := s.GetEphemeralChildExecutorFailure(ctx, job.ID)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	history, err := s.GetEphemeralChildHistory(ctx, job.ID)
	mustNil(t, err)
	// JSON reflection lets RED assert the missing projection behavior, not a missing type.
	raw, err := json.Marshal(history)
	mustNil(t, err)
	var fields map[string]json.RawMessage
	mustNil(t, json.Unmarshal(raw, &fields))
	expected, err := json.Marshal(fact)
	mustNil(t, err)
	if string(fields["ExecutorFailure"]) != string(expected) {
		t.Fatalf("history lost immutable executor failure: %s", raw)
	}
	if history.ChildID != child.ID || history.Failure != nil || history.ResultSeal != nil {
		t.Fatalf("history fabricated a result: %+v", history)
	}
	group, err := s.EphemeralChildHistoryAtStep(ctx, job.Parent.Principal.SwarmID, job.Parent.Sequence)
	mustNil(t, err)
	if !reflect.DeepEqual(group, []EphemeralChildHistory{history}) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("history disagrees or mutated data")
	}
	setActor(t, s, child.Executor.ActorUID, "incarnation=incarnation+1")
	retained, err := s.GetEphemeralChildHistory(ctx, job.ID)
	mustNil(t, err)
	raw, err = json.Marshal(retained)
	mustNil(t, err)
	mustNil(t, json.Unmarshal(raw, &fields))
	if string(fields["ExecutorFailure"]) != string(expected) || retained.Inventory != nil || retained.InventoryUnavailable != "incarnation_changed" {
		t.Fatal("current inventory erased historical decision")
	}
}
