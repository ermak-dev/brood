package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// FEATURE: Атомарный резерв всей текущей группы до первого Create.
// SPEC: Неполная группа и нехватка capacity сохраняют FIFO; любой отказ
// откатывает все новые actors/allocations/grants. Cleanup остаётся после join.

// IMPLEMENTATION: Общая FULL-транзакция переиспользует per-intent admission.
// RESULT: IMPLEMENTED — атомарный резерв, точные retries и отказ без изменений FIFO.
// STATUS: DONE
func allocateGroup(t *testing.T, s *Store, p SwarmChildParent, inputs map[string]EphemeralActorReservation, pools map[string]ManagedPool, auth SwarmChildAuthorize, admit EphemeralActorAdmission) ([]EphemeralChildAllocation, error) {
	t.Helper()
	return s.AllocateEphemeralChildGroup(context.Background(), p, inputs, pools, auth, admit)
}

func groupAllocationFixture(t *testing.T, capacity int) (*Store, EphemeralChildIntent, EphemeralChildIntent, map[string]EphemeralActorReservation, map[string]ManagedPool) {
	t.Helper()
	s, first := allocationFixture(t, 2)
	second, err := s.EnqueueEphemeralChild(context.Background(), first.Parent, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "second-job", "Second task", "pool", childAllow)
	mustNil(t, err)
	for range capacity {
		addEphemeralCapacity(t, s)
	}
	pool, err := s.GetManagedPool(context.Background(), "pool")
	mustNil(t, err)
	return s, first, second, map[string]EphemeralActorReservation{first.ID: ephemeralReservation("group-one", 303), second.ID: ephemeralReservation("group-two", 304)}, map[string]ManagedPool{"pool": pool}
}

func TestEphemeralGroupAllocationCapacityIsAtomic(t *testing.T) {
	s, first, _, inputs, pools := groupAllocationFixture(t, 1)
	before := databaseRows(t, s.db)
	queued, err := s.QueuedEphemeralChildIntents(context.Background(), 0)
	mustNil(t, err)
	got, err := allocateGroup(t, s, first.Parent, inputs, pools, childAllow, ephemeralAdmissionAllow)
	if !errors.Is(err, ErrPoolCapacity) || len(got) != 0 {
		t.Fatalf("want capacity wait, got %v %v", got, err)
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("group shortage committed a partial allocation")
	}
	after, err := s.QueuedEphemeralChildIntents(context.Background(), 0)
	mustNil(t, err)
	if !reflect.DeepEqual(queued, after) {
		t.Fatal("group wait changed FIFO intents")
	}
}

func TestEphemeralGroupAllocationCommitsFullReservationsAndExactRetries(t *testing.T) {
	s, first, second, inputs, pools := groupAllocationFixture(t, 2)
	calls := 0
	admit := func(ctx context.Context, job EphemeralChildIntent, actor ManagedActor) error {
		calls++
		var mode int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode))
		if mode != 2 || job.Parent != first.Parent || actor.Resources != inputs[job.ID].Resources {
			t.Fatal("wrong admission scope/resources/durability")
		}
		var count int
		mustNil(t, s.conn(ctx).QueryRowContext(ctx, `SELECT COUNT(*) FROM ephemeral_child_allocations`).Scan(&count))
		if count != calls-1 {
			t.Fatal("admission cannot see earlier batch reservations")
		}
		return nil
	}
	got, err := allocateGroup(t, s, first.Parent, inputs, pools, childAllow, admit)
	mustNil(t, err)
	if len(got) != 2 || got[0].IntentID != first.ID || got[1].IntentID != second.ID || calls != 2 {
		t.Fatalf("wrong group allocations: %+v", got)
	}
	for _, a := range got {
		grant, err := s.GetEphemeralChildCreateGrant(context.Background(), a.IntentID)
		mustNil(t, err)
		if grant.State != "queued" || grant.ReservedAt != "" {
			t.Fatal("batch issued physical permission")
		}
	}
	before := databaseRows(t, s.db)
	again, err := allocateGroup(t, s, first.Parent, inputs, pools, childAllow, admit)
	mustNil(t, err)
	if !reflect.DeepEqual(got, again) || calls != 2 || !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("retry changed retained reservations")
	}
	changed := inputs[first.ID]
	changed.CTID++
	inputs[first.ID] = changed
	if _, err := allocateGroup(t, s, first.Parent, inputs, pools, childAllow, admit); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatal("retry retargeted reservation")
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("retry modified retained history")
	}
}

func TestEphemeralGroupAllocationActualMembersAndRetainedReservations(t *testing.T) {
	ctx := context.Background()
	t.Run("actual-member", func(t *testing.T) {
		s, intent := allocationFixture(t, 2)
		addEphemeralCapacity(t, s)
		executor := intent.Parent.Principal
		executor.ActorUID = "b"
		reserveChild(t, s, intent.Parent, executor, "actual-job")
		pool, err := s.GetManagedPool(ctx, "pool")
		mustNil(t, err)
		got, err := allocateGroup(t, s, intent.Parent, map[string]EphemeralActorReservation{intent.ID: ephemeralReservation("new-child", 303)}, map[string]ManagedPool{"pool": pool}, childAllow, ephemeralAdmissionAllow)
		mustNil(t, err)
		if len(got) != 1 || got[0].IntentID != intent.ID {
			t.Fatal("actual member did not account for expected slot")
		}
	})
	t.Run("registered-intent-counted-once", func(t *testing.T) {
		s, first, second, inputs, pools := groupAllocationFixture(t, 2)
		allocated, err := s.AllocateEphemeralChild(ctx, first.ID, inputs[first.ID], childAllow, ephemeralAdmissionAllow)
		mustNil(t, err)
		_, err = s.ReserveEphemeralChildCreate(ctx, first.ID, childAllow)
		mustNil(t, err)
		setActor(t, s, allocated.ActorUID, "phase='running',stage='done',operation='start'")
		attestForRegistration(t, s, first)
		_, err = s.RegisterEphemeralChild(ctx, first.ID, childAllow)
		mustNil(t, err)
		got, err := allocateGroup(t, s, first.Parent, inputs, pools, childAllow, ephemeralAdmissionAllow)
		mustNil(t, err)
		if len(got) != 2 || got[0] != allocated || got[1].IntentID != second.ID {
			t.Fatal("registered intent was double-counted or replaced")
		}
		grant, err := s.GetEphemeralChildCreateGrant(ctx, first.ID)
		mustNil(t, err)
		if grant.State != "reserved" || grant.ReservedAt == "" || !grant.Replay {
			t.Fatal("batch reset an already issued Create")
		}
		before := databaseRows(t, s.db)
		for _, id := range []string{first.ID, second.ID} {
			if _, err := s.BeginEphemeralChildCleanup(ctx, id, childAllow); err == nil {
				t.Fatal("group reservation authorized cleanup before aggregate join")
			}
		}
		if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
			t.Fatal("early cleanup changed group inventory")
		}
	})
	t.Run("retained-reservation-omitted", func(t *testing.T) {
		s, first, second, inputs, pools := groupAllocationFixture(t, 2)
		allocated, err := s.AllocateEphemeralChild(ctx, first.ID, inputs[first.ID], childAllow, ephemeralAdmissionAllow)
		mustNil(t, err)
		delete(inputs, first.ID)
		got, err := allocateGroup(t, s, first.Parent, inputs, pools, childAllow, ephemeralAdmissionAllow)
		mustNil(t, err)
		if len(got) != 1 || got[0].IntentID != second.ID {
			t.Fatal("did not allocate all remaining work")
		}
		retained, err := s.GetEphemeralChildAllocation(ctx, first.ID)
		mustNil(t, err)
		if retained != allocated {
			t.Fatal("omitted reservation was replaced")
		}
	})
}

func TestEphemeralGroupAllocationProjectAdmissionCountsNativeAndEarlierReservations(t *testing.T) {
	s, first, _, inputs, pools := groupAllocationFixture(t, 2)
	before := databaseRows(t, s.db)
	denied := errors.New("native plus managed project capacity exhausted")
	calls := 0
	got, err := allocateGroup(t, s, first.Parent, inputs, pools, childAllow, func(ctx context.Context, _ EphemeralChildIntent, _ ManagedActor) error {
		calls++
		actors, err := s.ListManagedActors(ctx, first.Parent.Principal.SwarmID)
		if err != nil {
			return err
		}
		const nativeUsage, projectLimit = 1, 4
		if nativeUsage+len(actors)+1 > projectLimit {
			return denied
		}
		return nil
	})
	if !errors.Is(err, denied) || len(got) != 0 || calls != 2 {
		t.Fatalf("full project admission not enforced: %v %v calls=%d", got, err, calls)
	}
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("project denial committed earlier group reservation")
	}
}

func TestEphemeralGroupAllocationResourceShortagesStayQueued(t *testing.T) {
	for _, dimension := range []string{"cpu", "memory", "disk"} {
		t.Run(dimension, func(t *testing.T) {
			s, first, _, inputs, pools := groupAllocationFixture(t, 2)
			ctx := context.Background()
			pool := pools["pool"]
			_, used, err := s.managedPoolUsage(ctx, pool.UID)
			mustNil(t, err)
			extra := inputs[first.ID].Resources
			switch dimension {
			case "cpu":
				pool.Resources.CPUMillicores = used.CPUMillicores + extra.CPUMillicores
			case "memory":
				pool.Resources.MemoryBytes = used.MemoryBytes + extra.MemoryBytes
			case "disk":
				pool.Resources.DiskBytes = used.DiskBytes + extra.DiskBytes
			}
			pool, err = s.UpdateManagedPool(ctx, pool, pool.Revision)
			mustNil(t, err)
			pools["pool"] = pool
			before := databaseRows(t, s.db)
			got, err := allocateGroup(t, s, first.Parent, inputs, pools, childAllow, ephemeralAdmissionAllow)
			if !errors.Is(err, ErrPoolCapacity) || len(got) != 0 {
				t.Fatalf("resource shortage not waiting: %v %v", got, err)
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("resource wait changed FIFO or inventory")
			}
		})
	}
}

func TestEphemeralGroupAllocationRetryReauthorizesCurrentScope(t *testing.T) {
	for _, change := range []string{"nil-authority", "nil-admission", "stopped", "parent", "issued-create-retarget"} {
		t.Run(change, func(t *testing.T) {
			s, first, _, inputs, pools := groupAllocationFixture(t, 2)
			ctx := context.Background()
			_, err := allocateGroup(t, s, first.Parent, inputs, pools, childAllow, ephemeralAdmissionAllow)
			mustNil(t, err)
			auth := SwarmChildAuthorize(childAllow)
			admit := EphemeralActorAdmission(ephemeralAdmissionAllow)
			p := first.Parent
			switch change {
			case "nil-authority":
				auth = nil
			case "nil-admission":
				admit = nil
			case "stopped":
				sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
				mustNil(t, err)
				_, err = s.StopSwarm(ctx, sw.ID, sw.OwnerID, allow)
				mustNil(t, err)
			case "parent":
				p.Principal.Incarnation++
			case "issued-create-retarget":
				_, err := s.ReserveEphemeralChildCreate(ctx, first.ID, childAllow)
				mustNil(t, err)
				r := inputs[first.ID]
				r.CTID += 10
				r.UID = "replacement"
				r.OperationID = "replacement-create"
				inputs[first.ID] = r
			}
			before := databaseRows(t, s.db)
			got, err := allocateGroup(t, s, p, inputs, pools, auth, admit)
			if err == nil || len(got) != 0 {
				t.Fatal("retry bypassed authority/current scope or retargeted an issued Create")
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refused retry changed retained reservations")
			}
		})
	}
}

func TestEphemeralGroupAllocationConcurrentExactRetries(t *testing.T) {
	s, first, _, inputs, pools := groupAllocationFixture(t, 2)
	var wg sync.WaitGroup
	results := make(chan []EphemeralChildAllocation, 6)
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.AllocateEphemeralChildGroup(context.Background(), first.Parent, inputs, pools, childAllow, ephemeralAdmissionAllow)
			results <- got
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		mustNil(t, err)
	}
	var original []EphemeralChildAllocation
	for got := range results {
		if len(got) != 2 {
			t.Fatal("concurrent retry lost a group slot")
		}
		if original == nil {
			original = got
		} else if !reflect.DeepEqual(got, original) {
			t.Fatal("concurrent retry changed group identity")
		}
	}
	actors, err := s.ListManagedActors(context.Background(), "")
	mustNil(t, err)
	if len(actors) != 4 {
		t.Fatal("concurrent group retry leaked actor capacity")
	}
}

func TestEphemeralGroupAllocationRefusalsRollbackEverything(t *testing.T) {
	for _, kind := range []string{"missing-intent", "missing-reservation", "foreign-reservation", "pool-missing", "pool-revision", "pool-resources", "pool-template", "pool-source", "nil-authority", "nil-admission", "late-authority", "late-admission", "late-insert-failure", "late-insert-ignored", "late-grant-ignored", "late-network-ignored", "duplicate-uid", "duplicate-ctid", "stale-parent", "nested"} {
		t.Run(kind, func(t *testing.T) {
			s, first, second, inputs, pools := groupAllocationFixture(t, 2)
			p := first.Parent
			auth := SwarmChildAuthorize(childAllow)
			admit := EphemeralActorAdmission(ephemeralAdmissionAllow)
			denied := errors.New("project or local authority denied")
			switch kind {
			case "missing-intent":
				// Отдельная группа с известным total, но ещё без второго intent.
				s, first = allocationFixture(t, 2)
				p = first.Parent
				addEphemeralCapacity(t, s)
				addEphemeralCapacity(t, s)
				pool, err := s.GetManagedPool(context.Background(), "pool")
				mustNil(t, err)
				pools = map[string]ManagedPool{"pool": pool}
				inputs = map[string]EphemeralActorReservation{first.ID: ephemeralReservation("only", 303)}
			case "missing-reservation":
				delete(inputs, second.ID)
			case "foreign-reservation":
				inputs["foreign"] = ephemeralReservation("foreign", 305)
			case "pool-missing":
				delete(pools, "pool")
			case "pool-revision":
				pool := pools["pool"]
				pool.Revision++
				pools["pool"] = pool
			case "pool-resources":
				pool := pools["pool"]
				pool.Resources.MemoryBytes++
				pools["pool"] = pool
			case "pool-template":
				pool := pools["pool"]
				pool.Template.Profile = "changed"
				pools["pool"] = pool
			case "pool-source":
				pool := pools["pool"]
				pool.Source.Node = "other-node"
				pools["pool"] = pool
			case "nil-authority":
				auth = nil
			case "nil-admission":
				admit = nil
			case "late-authority":
				auth = func(_ context.Context, a SwarmChildAuthorization) error {
					if a.RequestID == second.RequestID {
						return denied
					}
					return nil
				}
			case "late-admission":
				admit = func(_ context.Context, j EphemeralChildIntent, _ ManagedActor) error {
					if j.ID == second.ID {
						return denied
					}
					return nil
				}
			case "late-insert-failure", "late-insert-ignored", "late-grant-ignored":
				table, action := "ephemeral_child_allocations", "ABORT,'synthetic late failure'"
				if kind != "late-insert-failure" {
					action = "IGNORE"
				}
				if kind == "late-grant-ignored" {
					table = "ephemeral_child_create_grants"
				}
				_, err := s.db.Exec(`CREATE TRIGGER test_group_failure BEFORE INSERT ON ` + table + ` WHEN NEW.intent_id='` + second.ID + `' BEGIN SELECT RAISE(` + action + `); END`)
				mustNil(t, err)
			case "late-network-ignored":
				_, err := s.db.Exec(`CREATE TRIGGER test_group_network_ignore BEFORE INSERT ON managed_actor_networks WHEN NEW.actor_uid='group-two' BEGIN SELECT RAISE(IGNORE); END`)
				mustNil(t, err)
			case "duplicate-uid":
				r := inputs[second.ID]
				r.UID = inputs[first.ID].UID
				inputs[second.ID] = r
			case "duplicate-ctid":
				r := inputs[second.ID]
				r.CTID = inputs[first.ID].CTID
				inputs[second.ID] = r
			case "stale-parent":
				p.RequestID = "stale-parent"
			}
			before := databaseRows(t, s.db)
			var got []EphemeralChildAllocation
			var err error
			if kind == "nested" {
				mustNil(t, s.WithTx(context.Background(), func(ctx context.Context) error {
					got, err = s.AllocateEphemeralChildGroup(ctx, p, inputs, pools, auth, admit)
					return nil
				}))
				if !errors.Is(err, ErrDurableTransactionNested) {
					t.Fatalf("nested allocation: %v", err)
				}
			} else {
				got, err = allocateGroup(t, s, p, inputs, pools, auth, admit)
			}
			if err == nil || len(got) != 0 {
				t.Fatalf("unsafe group accepted: %v %v", got, err)
			}
			if kind == "missing-intent" && !errors.Is(err, ErrEphemeralChildGroupPending) {
				t.Fatalf("missing expected intent was not waiting: %v", err)
			}
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("refused group changed inventory, queue or history")
			}
		})
	}
}
