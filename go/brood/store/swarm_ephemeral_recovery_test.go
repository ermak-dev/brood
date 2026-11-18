package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// FEATURE: Read-only FIFO recovery выделенных ephemeral jobs после рестарта.
// SPEC: Невыделенные, удалённые и чужие incarnation не попадают в страницу;
// insertion cursor стабилен, чтение не создаёт ни grant, ни terminal result.
func allocatedRecoveryPage(t *testing.T, s *Store, after int64) ([]EphemeralChildIntent, error) {
	t.Helper()
	return s.AllocatedEphemeralChildIntents(context.Background(), after)
}

func TestAllocatedEphemeralRecoveryReadRetainsFIFOWithoutEffects(t *testing.T) {
	ctx := context.Background()
	s, first := allocationFixture(t, 2)
	page, err := allocatedRecoveryPage(t, s, 0)
	mustNil(t, err)
	if len(page) != 0 {
		t.Fatal("unallocated job appeared in physical recovery")
	}
	second, err := s.EnqueueEphemeralChild(ctx, first.Parent, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "second-recovery", "Second recovery task", "pool", childAllow)
	mustNil(t, err)
	for _, job := range []EphemeralChildIntent{first, second} {
		addEphemeralCapacity(t, s)
		_, err := s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation(job.ID, 303+int(job.QueuePosition)), childAllow, ephemeralAdmissionAllow)
		mustNil(t, err)
	}
	before := databaseRows(t, s.db)
	page, err = allocatedRecoveryPage(t, s, 0)
	mustNil(t, err)
	if !reflect.DeepEqual(page, []EphemeralChildIntent{first, second}) {
		t.Fatalf("recovery lost stable insertion order: %+v", page)
	}
	page, err = allocatedRecoveryPage(t, s, first.QueuePosition)
	mustNil(t, err)
	if !reflect.DeepEqual(page, []EphemeralChildIntent{second}) {
		t.Fatalf("recovery repeated an earlier cursor: %+v", page)
	}
	page, err = allocatedRecoveryPage(t, s, second.QueuePosition)
	mustNil(t, err)
	if len(page) != 0 {
		t.Fatal("recovery cursor did not reach the end")
	}
	if _, err = allocatedRecoveryPage(t, s, -1); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("negative cursor accepted: %v", err)
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("recovery read mutated retained evidence")
	}
}

func TestAllocatedEphemeralRecoveryReadDoesNotAdoptInventory(t *testing.T) {
	for _, scenario := range []string{"deleted", "new-incarnation", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, job := allocationFixture(t, 1)
			addEphemeralCapacity(t, s)
			allocation, err := s.AllocateEphemeralChild(ctx, job.ID, ephemeralReservation("recovery-actor", 303), childAllow, ephemeralAdmissionAllow)
			mustNil(t, err)
			switch scenario {
			case "deleted":
				_, err = s.db.Exec(`UPDATE managed_actors SET phase='deleted' WHERE uid=?`, allocation.ActorUID)
			case "new-incarnation":
				_, err = s.db.Exec(`UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, allocation.ActorUID)
			case "missing":
				_, err = s.db.Exec(`DELETE FROM managed_actors WHERE uid=?`, allocation.ActorUID)
			}
			mustNil(t, err)
			before := databaseRows(t, s.db)
			page, err := allocatedRecoveryPage(t, s, 0)
			mustNil(t, err)
			if len(page) != 0 || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatalf("recovery adopted or repaired unrelated inventory: %+v", page)
			}
		})
	}
}
