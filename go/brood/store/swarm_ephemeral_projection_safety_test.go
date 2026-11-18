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

func TestEphemeralHistoryProjectionKeepsColdReservationSeparateFromProof(t *testing.T) {
	ctx := context.Background()
	s, intent := createGrantFixture(t)
	before := databaseRows(t, s.db)
	history, err := s.GetEphemeralChildHistory(ctx, intent.ID)
	mustNil(t, err)
	if history.Intent != intent || history.Allocation == nil || history.CreateGrant == nil || history.CreateGrant.State != "queued" || history.Attestation != nil || history.ChildID != "" || history.ResultSeal != nil || history.Inventory == nil || history.Inventory.Phase != "provisioning" || history.Inventory.Operation != "create" {
		t.Fatalf("cold history fabricated source or child execution: %+v", history)
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("history read consumed a create grant or changed inventory")
	}
	_, err = s.ReserveEphemeralChildCreate(ctx, intent.ID, childAllow)
	mustNil(t, err)
	before = databaseRows(t, s.db)
	history, err = s.GetEphemeralChildHistory(ctx, intent.ID)
	mustNil(t, err)
	if history.CreateGrant == nil || history.CreateGrant.State != "reserved" || !history.CreateGrant.Replay || history.Attestation != nil || history.ChildID != "" || history.ResultSeal != nil || history.Inventory.Phase != "provisioning" || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("reserved may-have-issued grant became a creation/source/completion receipt")
	}
	encoded, err := json.Marshal(history)
	mustNil(t, err)
	for _, field := range []string{"OwnerNonce", "CTID", "UPID", "lease", "Lease", "challenge", "signature", "Token"} {
		if strings.Contains(string(encoded), `"`+field+`"`) {
			t.Fatalf("projection exposed private physical or root field %s", field)
		}
	}
}

func TestEphemeralHistoryProjectionRetainsCompletedFactsAfterInventoryRemoval(t *testing.T) {
	ctx := context.Background()
	s, intent, child, _ := settledEphemeralResult(t)
	mustNil(t, s.SealEphemeralChildResult(ctx, intent.ID, childAllow))
	before := databaseRows(t, s.db)
	history, err := s.GetEphemeralChildHistory(ctx, intent.ID)
	mustNil(t, err)
	if history.ChildID != child.ID || history.Attestation == nil || history.Attestation.State != "attested" || history.ResultSeal == nil || history.ResultSeal.ChildID != child.ID || history.Inventory == nil {
		t.Fatalf("registered/result history disappeared: %+v", history)
	}
	jobs, err := s.EphemeralChildHistoryAtStep(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Sequence)
	mustNil(t, err)
	if !reflect.DeepEqual(jobs, []EphemeralChildHistory{history}) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("step projection lost exact history or changed facts")
	}
	// Проверяется только history при отсутствии inventory, не физический cleanup.
	_, err = s.db.Exec(`DELETE FROM managed_actors WHERE uid=?`, child.Executor.ActorUID)
	mustNil(t, err)
	before = databaseRows(t, s.db)
	retained, err := s.GetEphemeralChildHistory(ctx, intent.ID)
	mustNil(t, err)
	history.Inventory, history.InventoryUnavailable = nil, "not_retained"
	if !reflect.DeepEqual(retained, history) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("missing inventory erased result facts or was fabricated as confirmed deletion")
	}
	trace, err := s.GetSwarmPassTrace(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Pass)
	mustNil(t, err)
	if !reflect.DeepEqual(trace.Steps[0].EphemeralJobs, []EphemeralChildHistory{retained}) || trace.Steps[0].Children.Children[0].Report != child.Report {
		t.Fatal("pass trace lost original completed child report/history")
	}
	for _, step := range trace.Steps[1:] {
		if len(step.EphemeralJobs) != 0 {
			t.Fatal("job leaked into another parent sequence")
		}
	}
}

func TestEphemeralHistoryProjectionNeverAttachesForeignCurrentIncarnation(t *testing.T) {
	for _, change := range []string{"incarnation", "scope"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s, intent := createGrantFixture(t)
			want := "incarnation_changed"
			if change == "incarnation" {
				setActor(t, s, "isolated", "incarnation=2,name='foreign-current-name'")
			} else {
				setActor(t, s, "isolated", "atespace_uid='foreign',name='foreign-current-name'")
				want = "scope_changed"
			}
			before := databaseRows(t, s.db)
			history, err := s.GetEphemeralChildHistory(ctx, intent.ID)
			mustNil(t, err)
			encoded, err := json.Marshal(history)
			mustNil(t, err)
			if history.Inventory != nil || history.InventoryUnavailable != want || history.Allocation.ActorIncarnation != 1 || strings.Contains(string(encoded), "foreign-current-name") || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("foreign inventory was attached to an old allocation")
			}
		})
	}
}

func TestEphemeralHistoryProjectionRefusesOverBoundWithoutPartialRows(t *testing.T) {
	ctx := context.Background()
	s, intent := allocationFixture(t, 1)
	_, err := s.db.Exec(`DROP TRIGGER ephemeral_child_intent_insert_guard`)
	mustNil(t, err)
	query := `INSERT INTO ephemeral_child_intents (` + ephemeralChildIntentCols + `) SELECT ?,swarm_id,parent_sequence,pass,project_id,atespace_uid,parent_uid,parent_incarnation,parent_request_id,?,payload,pool_uid,source_fingerprint,template,source_commit,origin,created_at FROM ephemeral_child_intents WHERE id=?`
	for index := 1; index <= MaxSwarmChildren; index++ {
		_, err := s.db.Exec(query, fmt.Sprintf("extra-%d", index), fmt.Sprintf("request-%d", index), intent.ID)
		mustNil(t, err)
	}
	before := databaseRows(t, s.db)
	rows, err := s.EphemeralChildHistoryAtStep(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Sequence)
	if !errors.Is(err, ErrPassTraceLimit) || rows != nil {
		t.Fatalf("over-bound history silently truncated: count=%d err=%v", len(rows), err)
	}
	trace, err := s.GetSwarmPassTrace(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Pass)
	if !errors.Is(err, ErrPassTraceLimit) || !reflect.DeepEqual(trace, SwarmPassTrace{}) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("over-bound pass exposed partial history or changed records")
	}
}

func TestEphemeralHistoryProjectionCancellationAndMissingFacts(t *testing.T) {
	ctx := context.Background()
	s, intent := allocationFixture(t, 1)
	before := databaseRows(t, s.db)
	if _, err := s.GetEphemeralChildHistory(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing job invented history")
	}
	rows, err := s.EphemeralChildHistoryAtStep(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Sequence+1)
	mustNil(t, err)
	if rows == nil || len(rows) != 0 {
		t.Fatal("empty parent sequence acquired another step's jobs")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	one, err := s.GetEphemeralChildHistory(cancelled, intent.ID)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(one, EphemeralChildHistory{}) {
		t.Fatal("cancelled history read exposed partial data")
	}
	rows, err = s.EphemeralChildHistoryAtStep(cancelled, intent.Parent.Principal.SwarmID, intent.Parent.Sequence)
	if !errors.Is(err, context.Canceled) || rows != nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("cancelled list exposed partial data or mutated records")
	}
}
