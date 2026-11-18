package server

// FEATURE: Атомарный metadata admission всей группы эфемерных заданий.
// SPEC: Все слоты до Create, общий FULL project accounting и бюджет VMID;
// нехватка не оставляет частичных строк, retained retries не выбирают новые ID.
// IMPLEMENTATION: Общий project gate, внешний native-снимок, затем один FULL
// group reserve с повторным расчётом каждого нового актора через transaction ctx.
// RESULT: IMPLEMENTED — focused normal и race проходят, single-intent API сохранён.
// STATUS: DONE

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

func allocateQueuedGroupWithHint(s *Server, ctx context.Context, intentID string, hint, maxChecks int) ([]store.EphemeralChildAllocation, int, error) {
	intent, err := s.store.GetEphemeralChildIntent(ctx, intentID)
	if err != nil {
		return nil, 0, err
	}
	pool, err := s.store.GetManagedPool(ctx, intent.PoolUID)
	if err != nil {
		return nil, 0, err
	}
	allocations, cursors, err := s.allocateQueuedEphemeralGroup(ctx, intentID, map[string]int{pool.Source.ClusterIdentity: hint}, maxChecks)
	return allocations, cursors[pool.Source.ClusterIdentity], err
}

func groupCapacityFixture(t *testing.T, complete bool, automaticEphemeral ...bool) (*dispatchServer, []store.EphemeralChildIntent, *autoCapacityPVE) {
	t.Helper()
	d, first, p := autoCapacityFixture(t, automaticEphemeral...)
	intents := []store.EphemeralChildIntent{first}
	if complete {
		next, err := d.srv.store.EnqueueEphemeralChild(context.Background(), first.Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "group-second", "Second task", first.PoolUID, d.srv.managed.childAuthority)
		if err != nil {
			t.Fatal(err)
		}
		intents = append(intents, next)
	}
	return d, intents, p
}

func TestEphemeralGroupCapacityCompleteAndRetained(t *testing.T) {
	ctx := context.Background()
	d, intents, p := groupCapacityFixture(t, true)
	createSQL(t, d, `CREATE TRIGGER group_full BEFORE INSERT ON managed_actors WHEN NEW.actor_class='ephemeral' AND (SELECT synchronous FROM pragma_synchronous)<>2 BEGIN SELECT RAISE(ABORT,'group requires FULL'); END`)
	p.beforeConfirm = func(context.Context) error {
		createSQL(t, d, `UPDATE users SET username=username WHERE id=?`, d.master.DelegatorID)
		return nil
	}
	got, hint, err := allocateQueuedGroupWithHint(d.srv, ctx, intents[0].ID, 900, 4)
	if err != nil || len(got) != 2 || hint != 902 {
		t.Fatalf("group: %+v hint=%d %v", got, hint, err)
	}
	ids := map[int]bool{}
	uids := map[string]bool{}
	nonces := map[string]bool{}
	ops := map[string]bool{}
	for i, a := range got {
		if a.IntentID != intents[i].ID {
			t.Fatalf("wrong order: %+v", got)
		}
		actor, err := d.srv.store.GetManagedActor(ctx, a.ActorUID)
		if err != nil {
			t.Fatal(err)
		}
		if ids[actor.CTID] || uids[actor.UID] || nonces[actor.OwnerNonce] || ops[actor.OperationID] || actor.OwnerNonce == "" || actor.OperationID == "" {
			t.Fatalf("reused identity: %+v", actor)
		}
		ids[actor.CTID] = true
		uids[actor.UID] = true
		nonces[actor.OwnerNonce] = true
		ops[actor.OperationID] = true
		grant, err := d.srv.store.GetEphemeralChildCreateGrant(ctx, a.IntentID)
		if err != nil || grant.State != "queued" {
			t.Fatalf("issued Create: %+v %v", grant, err)
		}
	}
	calls, checks := p.nextCalls, len(p.checked)
	again, _, err := allocateQueuedGroupWithHint(d.srv, ctx, intents[1].ID, 0, 1)
	if err != nil || !reflect.DeepEqual(got, again) || p.nextCalls != calls || len(p.checked) != checks {
		t.Fatalf("retained replay: %+v %v", again, err)
	}
	if effects := providerEffectsOf(p.managedFakePVE); effects.creates != 0 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("physical effects: %+v", effects)
	}
}

func TestEphemeralGroupCapacityRetainedRechecks(t *testing.T) {
	for _, change := range []string{"selector", "authority", "role", "stopped", "pool-snapshot"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			d, intents, p := groupCapacityFixture(t, true)
			saved, _, err := allocateQueuedGroupWithHint(d.srv, ctx, intents[0].ID, 900, 4)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "selector":
				if err := d.srv.store.SetProjectCapacity(ctx, d.projectID(t), store.ProjectCapacity{WorkerSelector: map[string]string{"missing": "required"}}); err != nil {
					t.Fatal(err)
				}
			case "authority":
				if err := d.srv.store.SetUserAdmin(ctx, d.master.DelegatorID, false); err != nil {
					t.Fatal(err)
				}
			case "role":
				createSQL(t, d, `UPDATE swarm_role_bindings SET actor_uid=?,actor_incarnation=? WHERE swarm_id=? AND actor_uid=?`, d.worker.UID, d.worker.Incarnation, d.swarm, d.master.UID)
			case "stopped":
				d.stop()
			case "pool-snapshot":
				called := false
				d.fake.mu.Lock()
				d.fake.beforeResourceRead = func() {
					if called {
						return
					}
					called = true
					q, err := d.srv.store.GetManagedPool(ctx, intents[0].PoolUID)
					if err != nil {
						t.Error(err)
						return
					}
					q.Labels["during-native-read"] = "changed"
					if _, err := d.srv.store.UpdateManagedPool(ctx, q, q.Revision); err != nil {
						t.Error(err)
					}
				}
				d.fake.mu.Unlock()
			}
			calls, checks := p.nextCalls, len(p.checked)
			got, _, err := allocateQueuedGroupWithHint(d.srv, ctx, intents[0].ID, 0, 1)
			if err == nil || len(got) != 0 {
				t.Fatalf("retained bypassed current %s: %+v %v", change, got, err)
			}
			if p.nextCalls != calls || len(p.checked) != checks {
				t.Fatal("retained retry selected VMIDs")
			}
			for _, a := range saved {
				got, err := d.srv.store.GetEphemeralChildAllocation(ctx, a.IntentID)
				if err != nil || got != a {
					t.Fatalf("changed retained binding: %+v %v", got, err)
				}
			}
		})
	}
}

func TestEphemeralGroupCapacityPreflightsFirstNewJob(t *testing.T) {
	ctx := context.Background()
	d, intents, p := groupCapacityFixture(t, true)
	retained, _, err := d.srv.allocateQueuedEphemeralChild(ctx, intents[0].ID, 900, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.store.ReserveEphemeralChildCreate(ctx, intents[0].ID, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	three := int64(3)
	if err := d.srv.store.SetProjectCapacity(ctx, d.projectID(t), store.ProjectCapacity{MaxActors: &three}); err != nil {
		t.Fatal(err)
	}
	calls, checks := p.nextCalls, len(p.checked)
	got, _, err := allocateQueuedGroupWithHint(d.srv, ctx, intents[0].ID, 0, 4)
	if err == nil || len(got) != 0 || p.nextCalls != calls || len(p.checked) != checks {
		t.Fatalf("did not preflight actual new job: %+v %v", got, err)
	}
	if err := d.srv.store.SetProjectCapacity(ctx, d.projectID(t), store.ProjectCapacity{}); err != nil {
		t.Fatal(err)
	}
	got, _, err = allocateQueuedGroupWithHint(d.srv, ctx, intents[0].ID, 0, 4)
	if err != nil || len(got) != 2 || got[0] != retained {
		t.Fatalf("mixed retry retargeted issued binding: %+v %v", got, err)
	}
}

func TestEphemeralGroupCapacityEveryPoolRechecksCurrentSelector(t *testing.T) {
	ctx := context.Background()
	d, intents, p := groupCapacityFixture(t, false)
	first, err := d.srv.store.GetManagedPool(ctx, intents[0].PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := d.srv.managed.registry.pools[first.Connection]
	d.srv.managed.registry.pools["group-second-pool"] = cfg
	second, err := d.srv.managed.configuredPool("group-second-pool")
	if err != nil {
		t.Fatal(err)
	}
	second.UID = store.NewID()
	second, err = d.srv.store.CreateManagedPool(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.srv.store.EnqueueEphemeralChild(ctx, intents[0].Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "second-pool-job", "Second pool task", second.UID, d.srv.managed.childAuthority); err != nil {
		t.Fatal(err)
	}
	p.beforeConfirm = func(ctx context.Context) error {
		return d.srv.store.SetProjectCapacity(ctx, d.projectID(t), store.ProjectCapacity{WorkerSelector: map[string]string{"ate.dev/worker-pool": first.Name}})
	}
	got, _, err := allocateQueuedGroupWithHint(d.srv, ctx, intents[0].ID, 900, 4)
	if err == nil || len(got) != 0 {
		t.Fatalf("second pool bypassed current worker selector: %+v %v", got, err)
	}
	if len(p.checked) != 2 {
		t.Fatalf("did not reach transaction recheck for both pools: %v", p.checked)
	}
	if _, err := d.srv.store.GetEphemeralChildAllocation(ctx, intents[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("partial first pool: %v", err)
	}
}

func TestEphemeralGroupCapacityNativeMemberFillsSlot(t *testing.T) {
	ctx := context.Background()
	d, intents, _ := groupCapacityFixture(t, false)
	if _, err := d.srv.managed.createChild(ctx, d.master, childArgs(d, "native-slot")); err != nil {
		t.Fatal(err)
	}
	got, _, err := allocateQueuedGroupWithHint(d.srv, ctx, intents[0].ID, 900, 4)
	if err != nil || len(got) != 1 {
		t.Fatalf("native slot ignored: %+v %v", got, err)
	}
}

func TestEphemeralGroupCapacityRegisteredIntentCountedOnce(t *testing.T) {
	ctx := context.Background()
	d, first, child := ephemeralChildServerFixture(t)
	second, err := d.srv.store.EnqueueEphemeralChild(ctx, first.Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "after-registered", "Second task", first.PoolUID, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := d.srv.store.GetManagedPool(ctx, first.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := d.srv.managed.registry.pools[pool.Connection].runtime
	runtime.actors = pool.MaxActors
	runtime.cpuMillicores = pool.Resources.CPUMillicores
	runtime.memoryBytes = pool.Resources.MemoryBytes
	runtime.diskGiB = pool.Resources.DiskBytes / gib
	p := &autoCapacityPVE{managedFakePVE: &managedFakePVE{server: d.srv}, suggestion: 900, occupied: map[int]bool{}}
	d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return p, nil }
	got, _, err := allocateQueuedGroupWithHint(d.srv, ctx, second.ID, 0, 4)
	if err != nil || len(got) != 2 || got[0].ActorUID != child.Executor.ActorUID || len(p.checked) != 1 {
		t.Fatalf("registered intent double-counted/replaced: %+v %v", got, err)
	}
	join, err := d.srv.store.GetSwarmChildJoin(ctx, first.Parent, d.srv.managed.childAuthority)
	if err != nil || len(join.Children) != 1 || join.Children[0] != child {
		t.Fatalf("changed registered task: %+v %v", join, err)
	}
}

func TestEphemeralGroupCapacitySkipsHoldsAndSharesCollisionBudget(t *testing.T) {
	ctx := context.Background()
	d, intents, p := groupCapacityFixture(t, true)
	actors, err := d.srv.store.ListManagedActors(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	held := map[int]bool{}
	for _, a := range actors {
		held[a.CTID] = true
	}
	candidate := p.suggestion
	for held[candidate] {
		candidate++
	}
	collision := candidate + 1
	for held[collision] {
		collision++
	}
	p.occupied[collision] = true
	last := collision + 1
	for held[last] {
		last++
	}
	got, _, err := allocateQueuedGroupWithHint(d.srv, ctx, intents[0].ID, 0, 3)
	if err != nil || len(got) != 2 || p.nextCalls != 1 || !reflect.DeepEqual(p.checked, []int{candidate, collision, last}) {
		t.Fatalf("group scan checked holds/reused tentative IDs: %+v %v checks=%v next=%d", got, err, p.checked, p.nextCalls)
	}
}

func TestEphemeralGroupCapacityRefusalsAreAtomic(t *testing.T) {
	for _, scenario := range []string{"missing", "pool-one-slot", "actors", "cpu", "memory", "selector-change", "authority-change", "pool-change", "namespace-change", "budget"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			d, intents, p := groupCapacityFixture(t, scenario != "missing")
			sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
			if err != nil {
				t.Fatal(err)
			}
			template := inventoryTemplate(sw.AtespaceName, "group-native-base", "group-native-template", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
			d.fake.mu.Lock()
			d.fake.templates[sw.AtespaceName+"/group-native-base"] = template
			d.fake.actors[sw.AtespaceName+"/native"] = &ateapi.Actor{Metadata: fakeMeta("native", sw.AtespaceName), ActorTemplate: &ateapi.ObjectRef{Name: "group-native-base", Atespace: sw.AtespaceName}}
			d.fake.mu.Unlock()
			pool, err := d.srv.store.GetManagedPool(ctx, intents[0].PoolUID)
			if err != nil {
				t.Fatal(err)
			}
			r, err := managedActorResources(pool)
			if err != nil {
				t.Fatal(err)
			}
			cap := store.ProjectCapacity{}
			switch scenario {
			case "pool-one-slot":
				pool.MaxActors = 3
				if _, err = d.srv.store.UpdateManagedPool(ctx, pool, pool.Revision); err != nil {
					t.Fatal(err)
				}
				d.srv.managed.registry.pools[pool.Connection].runtime.actors = 3
			case "actors":
				n := int64(4)
				cap.MaxActors = &n
			case "cpu":
				n := d.master.Resources.CPUMillicores + d.worker.Resources.CPUMillicores + template.GetResources().GetLimits().GetCpuMillicores() + r.CPUMillicores
				cap.MaxCPUMillicores = &n
			case "memory":
				n := d.master.Resources.MemoryBytes + d.worker.Resources.MemoryBytes + template.GetResources().GetLimits().GetMemoryBytes() + r.MemoryBytes
				cap.MaxMemoryBytes = &n
			case "selector-change":
				p.beforeConfirm = func(ctx context.Context) error {
					return d.srv.store.SetProjectCapacity(ctx, sw.ProjectID, store.ProjectCapacity{WorkerSelector: map[string]string{"missing": "required"}})
				}
			case "authority-change":
				p.beforeConfirm = func(ctx context.Context) error { return d.srv.store.SetUserAdmin(ctx, d.master.DelegatorID, false) }
			case "pool-change":
				p.beforeConfirm = func(ctx context.Context) error {
					q, err := d.srv.store.GetManagedPool(ctx, pool.UID)
					if err != nil {
						return err
					}
					q.Labels["changed"] = "true"
					_, err = d.srv.store.UpdateManagedPool(ctx, q, q.Revision)
					return err
				}
			case "namespace-change":
				p.beforeConfirm = func(context.Context) error {
					d.fake.mu.Lock()
					d.fake.atespaces[sw.AtespaceName].Metadata.Uid = "replacement"
					d.fake.mu.Unlock()
					return nil
				}
			}
			if err := d.srv.store.SetProjectCapacity(ctx, sw.ProjectID, cap); err != nil {
				t.Fatal(err)
			}
			before, err := d.srv.store.ListManagedActors(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			budget := 4
			if scenario == "budget" {
				budget = 1
			}
			got, next, err := allocateQueuedGroupWithHint(d.srv, ctx, intents[0].ID, 900, budget)
			if err == nil || len(got) != 0 {
				t.Fatalf("partial or accepted group: %+v %v", got, err)
			}
			if scenario == "missing" && !errors.Is(err, store.ErrEphemeralChildGroupPending) {
				t.Fatalf("missing group: %v", err)
			}
			if scenario == "budget" && (len(p.checked) != 1 || next != 901) {
				t.Fatalf("shared budget: checks=%v next=%d", p.checked, next)
			}
			after, readErr := d.srv.store.ListManagedActors(ctx, "")
			if readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("partial actor rows: %v", readErr)
			}
			for _, intent := range intents {
				if _, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("partial allocation: %v", err)
				}
				if _, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("invented failure: %v", err)
				}
			}
			queued, err := d.srv.store.QueuedEphemeralChildIntents(ctx, 0)
			if err != nil || len(queued) != len(intents) {
				t.Fatalf("lost queue: %+v %v", queued, err)
			}
			if effects := providerEffectsOf(p.managedFakePVE); effects.creates != 0 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
				t.Fatalf("physical effects: %+v", effects)
			}
		})
	}
}
