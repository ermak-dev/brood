package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func ephemeralAdmissionAllow(context.Context, EphemeralChildIntent, ManagedActor) error { return nil }
func ephemeralReservation(uid string, ctid int) EphemeralActorReservation {
	return EphemeralActorReservation{UID: uid, CTID: ctid, OwnerNonce: "nonce-" + uid, OperationID: "create-" + uid, Resources: ManagedResources{CPUMillicores: 1000, MemoryBytes: 1024, DiskBytes: 2048}}
}
func allocationFixture(t *testing.T, total int) (*Store, EphemeralChildIntent) {
	t.Helper()
	s, p, _ := childFixture(t, "all", total, 0)
	intent, err := s.EnqueueEphemeralChild(context.Background(), p, SwarmChildJoinPolicy{Mode: "all", Total: total, Remaining: "retain"}, "allocation-job", "One isolated task", "pool", childAllow)
	mustNil(t, err)
	return s, intent
}
func addEphemeralCapacity(t *testing.T, s *Store) {
	t.Helper()
	p, err := s.GetManagedPool(context.Background(), "pool")
	mustNil(t, err)
	p.MaxActors++
	p.Resources.CPUMillicores += 1000
	p.Resources.MemoryBytes += 1024
	p.Resources.DiskBytes += 2048
	_, err = s.UpdateManagedPool(context.Background(), p, p.Revision)
	mustNil(t, err)
}

// Выделение #292 должно полностью записать UID/incarnation и create-intent
// до PVE; повтор не получает другого актора, а нехватка места оставляет очередь.
func TestEphemeralAllocationKeepsFullPoolQueuedWithoutPartialInventory(t *testing.T) {
	ctx := context.Background()
	s, intent := allocationFixture(t, 1)
	before := databaseRows(t, s.db)
	got, err := s.AllocateEphemeralChild(ctx, intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
	if !errors.Is(err, ErrPoolCapacity) || !reflect.DeepEqual(got, EphemeralChildAllocation{}) {
		t.Fatal("full pool was not retained as waiting allocation")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("capacity wait wrote partial actor or binding")
	}
	queued, err := s.QueuedEphemeralChildIntents(ctx, 0)
	mustNil(t, err)
	if len(queued) != 1 || queued[0].ID != intent.ID {
		t.Fatal("capacity wait lost queued child")
	}
}

func TestEphemeralAllocationCommitsOneDerivedActorAndExactRetry(t *testing.T) {
	ctx := context.Background()
	s, intent := allocationFixture(t, 1)
	addEphemeralCapacity(t, s)
	reservation := ephemeralReservation("isolated", 303)
	calls := 0
	admit := func(ctx context.Context, job EphemeralChildIntent, actor ManagedActor) error {
		calls++
		var syncMode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&syncMode))
		if syncMode != 2 {
			t.Fatal("allocation permission was not FULL-sync")
		}
		if job.ID != intent.ID || actor.SwarmID != intent.Parent.Principal.SwarmID || actor.PoolUID != intent.PoolUID || actor.Class != ActorClassEphemeral || actor.ParentUID != intent.Parent.Principal.ActorUID || actor.Origin != intent.Origin || actor.Resources != reservation.Resources {
			t.Fatal("project admission did not see the exact derived allocation")
		}
		return nil
	}
	allocated, err := s.AllocateEphemeralChild(ctx, intent.ID, reservation, childAllow, admit)
	mustNil(t, err)
	if allocated.IntentID != intent.ID || allocated.ActorUID != reservation.UID || allocated.ActorIncarnation != 1 || allocated.CreateOperationID != reservation.OperationID {
		t.Fatalf("wrong immutable allocation binding: %+v", allocated)
	}
	actor, err := s.GetManagedActor(ctx, reservation.UID)
	mustNil(t, err)
	if actor.Class != ActorClassEphemeral || actor.Role != "worker" || actor.ParentUID != intent.Parent.Principal.ActorUID || actor.SwarmID != intent.Parent.Principal.SwarmID || actor.AtespaceUID != intent.Parent.Principal.AtespaceUID || actor.PoolUID != intent.PoolUID || actor.Origin != intent.Origin || actor.Template != intent.Template || actor.Incarnation != 1 || actor.Stage != "create" || actor.Operation != "create" || actor.Phase != "provisioning" || !strings.HasPrefix(actor.Name, "e-") {
		t.Fatalf("allocation did not derive safe create inventory: %+v", actor)
	}
	before := databaseRows(t, s.db)
	repeated, err := s.AllocateEphemeralChild(ctx, intent.ID, reservation, childAllow, admit)
	mustNil(t, err)
	if !reflect.DeepEqual(allocated, repeated) || calls != 1 || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("exact retry readmitted resources or allocated another worker")
	}
	queued, err := s.QueuedEphemeralChildIntents(ctx, 0)
	mustNil(t, err)
	if len(queued) != 0 {
		t.Fatal("allocated job remained in unallocated FIFO")
	}
	changed := reservation
	changed.UID = "another-worker"
	_, err = s.AllocateEphemeralChild(ctx, intent.ID, changed, childAllow, admit)
	if !errors.Is(err, ErrSwarmChildConflict) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("allocation retry replaced worker identity")
	}
}

func TestEphemeralAllocationRequiresParentAndProjectAdmissionAndRefusesNestedEffects(t *testing.T) {
	ctx := context.Background()
	s, intent := allocationFixture(t, 1)
	addEphemeralCapacity(t, s)
	input := ephemeralReservation("isolated", 303)
	before := databaseRows(t, s.db)
	if _, err := s.AllocateEphemeralChild(ctx, intent.ID, input, nil, ephemeralAdmissionAllow); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatal("untrusted parent obtained an allocation")
	}
	if _, err := s.AllocateEphemeralChild(ctx, intent.ID, input, childAllow, nil); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatal("missing project admission obtained an allocation")
	}
	denied := errors.New("project resource limit is exhausted")
	if _, err := s.AllocateEphemeralChild(ctx, intent.ID, input, childAllow, func(context.Context, EphemeralChildIntent, ManagedActor) error { return denied }); !errors.Is(err, denied) {
		t.Fatal("project admission failure was ignored")
	}
	mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.AllocateEphemeralChild(ctx, intent.ID, input, childAllow, ephemeralAdmissionAllow)
		if !errors.Is(err, ErrDurableTransactionNested) {
			return errors.New("allocation returned effect permission before outer commit")
		}
		return nil
	}))
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("refused admission wrote inventory, binding or queue state")
	}
}

func TestEphemeralAllocationBindingFailureRollsBackCreateInventory(t *testing.T) {
	s, intent := allocationFixture(t, 1)
	addEphemeralCapacity(t, s)
	_, err := s.db.Exec(`CREATE TRIGGER test_allocation_failure BEFORE INSERT ON ephemeral_child_allocations BEGIN SELECT RAISE(ABORT,'synthetic binding failure'); END`)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	value, err := s.AllocateEphemeralChild(context.Background(), intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
	if err == nil || !reflect.DeepEqual(value, EphemeralChildAllocation{}) || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("binding failure committed a partial create operation or actor reservation")
	}
}

func TestEphemeralAllocationIsImmutableAndCannotBindForeignOrPersistentActor(t *testing.T) {
	s, intent := allocationFixture(t, 2)
	addEphemeralCapacity(t, s)
	allocated, err := s.AllocateEphemeralChild(context.Background(), intent.ID, ephemeralReservation("isolated", 303), childAllow, ephemeralAdmissionAllow)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	for _, query := range []string{
		`UPDATE ephemeral_child_allocations SET actor_uid='a' WHERE intent_id=?`,
		`DELETE FROM ephemeral_child_allocations WHERE intent_id=?`,
		`INSERT OR REPLACE INTO ephemeral_child_allocations SELECT * FROM ephemeral_child_allocations WHERE intent_id=?`,
	} {
		if _, err := s.db.Exec(query, intent.ID); err == nil {
			t.Fatal("worker allocation binding was rewritten")
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("immutable binding refusal changed rows")
		}
	}
	other, err := s.EnqueueEphemeralChild(context.Background(), intent.Parent, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "other", "Other child payload", "pool", childAllow)
	mustNil(t, err)
	before = databaseRows(t, s.db)
	if _, err := s.db.Exec(`INSERT INTO ephemeral_child_allocations(intent_id,actor_uid,actor_incarnation,create_operation_id,reservation_hash,created_at) VALUES(?,'a',1,'create-a',?,'now')`, other.ID, strings.Repeat("a", 64)); err == nil {
		t.Fatal("persistent actor was rebound as an ephemeral worker")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("foreign allocation refusal changed inventory")
	}
	read, err := s.GetEphemeralChildAllocation(context.Background(), intent.ID)
	mustNil(t, err)
	if !reflect.DeepEqual(allocated, read) {
		t.Fatal("historical allocation read changed worker identity")
	}
}

func TestEphemeralAllocationConcurrentExactRetriesCreateOneWorker(t *testing.T) {
	s, intent := allocationFixture(t, 1)
	addEphemeralCapacity(t, s)
	input := ephemeralReservation("isolated", 303)
	var wg sync.WaitGroup
	results := make(chan EphemeralChildAllocation, 6)
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := s.AllocateEphemeralChild(context.Background(), intent.ID, input, childAllow, ephemeralAdmissionAllow)
			results <- value
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		mustNil(t, err)
	}
	var original EphemeralChildAllocation
	for value := range results {
		if original.ActorUID == "" {
			original = value
		} else if !reflect.DeepEqual(value, original) {
			t.Fatal("concurrent identical claims returned different allocations")
		}
	}
	actors, err := s.ListManagedActors(context.Background(), "")
	mustNil(t, err)
	if len(actors) != 3 {
		t.Fatal("concurrent claims leaked actor capacity")
	}
}
