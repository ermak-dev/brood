package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// BUG: Failure cleanup навсегда держала положительно незапрошенный first-start.
// HYPOTHESIS: Незавершённая стадия отменялась только для queued original Create.
// SOLUTION: Только exact start provenance и положительный unsent marker допускают отмену.
// RESULT: FIXED — unknown/unproven, nil/denied authority и ignored writes остаются hold;
// полный Go/Python прогон и focused race проходят без переписывания исходных фактов.
// STATUS: FIXED
func TestEphemeralFailureCleanupCancelsOnlyProvenUnrequestedStart(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"known", "unknown", "unproven", "nil", "denied", "ignored-update"} {
		t.Run(scenario, func(t *testing.T) {
			s, job := lifecycleFixture(t, "start")
			actor, err := s.BeginEphemeralChildStart(ctx, job.ID, childAllow)
			mustNil(t, err)
			switch scenario {
			case "unknown":
				actor, err = s.ReserveEphemeralChildLifecycleStage(ctx, actor.UID, actor.Incarnation, actor.OperationID, "start", actor.Revision, childAllow)
				mustNil(t, err)
			case "unproven":
				setActor(t, s, actor.UID, "operation_id='unrecorded-start'")
				actor, err = s.GetManagedActor(ctx, actor.UID)
				mustNil(t, err)
			}
			mustNil(t, s.RecordEphemeralChildFailure(ctx, job.ID, "Explicit bootstrap validation rejection before Start", childAllow))
			_, err = s.JoinSwarmChildren(ctx, job.Parent, childAllow)
			mustNil(t, err)
			authorize := SwarmChildAuthorize(childAllow)
			if scenario == "nil" {
				authorize = nil
			}
			if scenario == "denied" {
				authorize = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			}
			if scenario == "ignored-update" {
				_, err := s.db.Exec(`CREATE TRIGGER ignore_cancel_start BEFORE UPDATE ON managed_actors BEGIN SELECT RAISE(IGNORE); END`)
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			cleanup, err := s.BeginEphemeralChildCleanup(ctx, job.ID, authorize)
			if scenario != "known" {
				if err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatalf("unsafe start replacement changed retained state: %+v %v", cleanup, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("known unrequested first-start could not enter failure cleanup: %v", err)
			}
			if cleanup.OperationID == actor.OperationID || cleanup.Operation != "delete" || cleanup.Stage != "stop" || cleanup.Unsent() != ManagedStopUnsent || cleanup.UPID != "" || cleanup.Incarnation != actor.Incarnation || cleanup.CTID != actor.CTID || cleanup.OwnerNonce != actor.OwnerNonce {
				t.Fatalf("cancellation invented a start receipt or changed physical identity: %+v", cleanup)
			}
			old, err := s.GetEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID)
			mustNil(t, err)
			next, err := s.GetEphemeralChildLifecycle(ctx, cleanup.UID, cleanup.Incarnation, cleanup.OperationID)
			mustNil(t, err)
			if old.Kind != "start" || next.Kind != "failure_cleanup" || old.IntentID != job.ID || next.IntentID != job.ID {
				t.Fatal("cancellation rewrote the original start provenance")
			}
			if _, err := s.GetEphemeralChildResultSeal(ctx, job.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("cancellation invented an actor result seal")
			}
			after := databaseRows(t, s.db)
			for _, table := range []string{"managed_actors", "ephemeral_child_lifecycles"} {
				delete(before, table)
				delete(after, table)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("start cancellation changed reports/grants/failure/physical claims")
			}
		})
	}
}
