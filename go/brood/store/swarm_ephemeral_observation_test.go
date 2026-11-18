package store

import (
	"context"
	"reflect"
	"testing"
)

// BUG: Read-only lifecycle check отказывал после регистрации готового ребёнка.
// HYPOTHESIS: Проверка наблюдения повторно применяет запрет нового first-start.
// SOLUTION: Только readonly action наблюдает registered membership; workspace,
// coordination и physical scope остаются, свежие grants исключения не получают.
// RESULT: FIXED — readonly/no writes и отказ fresh begin/reserve проверены в полном прогоне.
// STATUS: FIXED
func TestEphemeralStartObservationDoesNotRenewRegisteredExecution(t *testing.T) {
	ctx := context.Background()
	s, job := lifecycleFixture(t, "start")
	a, err := s.BeginEphemeralChildStart(ctx, job.ID, childAllow)
	mustNil(t, err)
	a, err = s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, a.Incarnation, a.OperationID, "start", a.Revision, childAllow)
	mustNil(t, err)
	a, err = s.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, "start", "observed-start-task")
	mustNil(t, err)
	a, err = s.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "start", "running")
	mustNil(t, err)
	attestForRegistration(t, s, job)
	_, err = s.RegisterEphemeralChild(ctx, job.ID, childAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	if err := s.CheckEphemeralChildLifecycle(ctx, a.UID, a.Incarnation, a.OperationID, childAllow); err != nil {
		t.Fatalf("registered ready executor lost read-only observation scope: %v", err)
	}
	if _, err := s.BeginEphemeralChildStart(ctx, job.ID, childAllow); err == nil {
		t.Fatal("observation renewed first-start intent after registration")
	}
	if _, err := s.ReserveEphemeralChildLifecycleStage(ctx, a.UID, a.Incarnation, a.OperationID, "start", a.Revision, childAllow); err == nil {
		t.Fatal("observation renewed stage effect permission after registration")
	}
	if err := s.CheckEphemeralChildLifecycle(ctx, a.UID, a.Incarnation, a.OperationID, nil); err == nil {
		t.Fatal("read-only observation bypassed authorization")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("read-only check or rejected grant changed retained rows")
	}
}
