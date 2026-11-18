package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// BUG: Отказанный executor удерживает capacity после успешного any/quorum join.
// HYPOTHESIS: Scope ошибочно требует failed outcome родителя вместо точного
// применённого server-computed результата join; ранний cleanup остаётся запрещён.
func TestEphemeralFailureCleanupAfterAppliedSuccessfulJoin(t *testing.T) {
	for _, mode := range []string{"any", "quorum"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			quorum := 0
			if mode == "quorum" {
				quorum = 1
			}
			s, parent, executor := childFixture(t, mode, 2, quorum)
			policy := SwarmChildJoinPolicy{Mode: mode, Total: 2, Quorum: quorum, Remaining: "retain"}
			failed, err := s.EnqueueEphemeralChild(ctx, parent, policy, "failed-job", "Failed isolated task", "pool", childAllow)
			mustNil(t, err)
			addEphemeralCapacity(t, s)
			allocation, err := s.AllocateEphemeralChild(ctx, failed.ID, ephemeralReservation("failed-worker", 303), childAllow, ephemeralAdmissionAllow)
			mustNil(t, err)
			mustNil(t, s.RecordEphemeralChildFailure(ctx, failed.ID, "Explicit trusted bootstrap rejection", childAllow))
			failure, err := s.GetEphemeralChildFailure(ctx, failed.ID)
			mustNil(t, err)
			finishChild(t, s, parent, executor, "successful-job", "success")
			before := databaseRows(t, s.db)
			if _, err = s.BeginEphemeralChildCleanup(ctx, failed.ID, childAllow); err == nil || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("computed but unapplied join permitted cleanup or changed evidence")
			}
			join, err := s.JoinSwarmChildren(ctx, parent, childAllow)
			mustNil(t, err)
			if join.Evaluation.State != "ready_success" || join.Evaluation.Open != 0 {
				t.Fatalf("fixture did not apply successful mixed join: %+v", join)
			}
			actor, err := s.BeginEphemeralChildCleanup(ctx, failed.ID, childAllow)
			if err != nil {
				t.Fatalf("failed member of applied %s success join cannot clean up: %v", mode, err)
			}
			if actor.UID != allocation.ActorUID || actor.Stage != "stop" || actor.Operation != "delete" || actor.DeletedAt != nil || actor.UPID != "" {
				t.Fatalf("cleanup released capacity or submitted an effect: %+v", actor)
			}
			history, err := s.GetEphemeralChildLifecycle(ctx, actor.UID, actor.Incarnation, actor.OperationID)
			mustNil(t, err)
			retainedFailure, err := s.GetEphemeralChildFailure(ctx, failed.ID)
			mustNil(t, err)
			visit, err := s.GetSwarmStep(ctx, parent.Principal.SwarmID, parent.Sequence)
			mustNil(t, err)
			if history.Kind != "failure_cleanup" || retainedFailure != failure || visit.Outcome == nil || *visit.Outcome != "success" || visit.Report != ChildJoinReport(join) {
				t.Fatalf("cleanup rewrote logical failure or parent outcome: history=%+v failure=%+v visit=%+v", history, retainedFailure, visit)
			}
			if _, err = s.GetEphemeralChildResultSeal(ctx, failed.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("failed member received a fake independent report seal")
			}
		})
	}
}
