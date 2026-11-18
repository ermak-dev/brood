package server

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

type autoCapacityPVE struct {
	*managedFakePVE
	suggestion    int
	nextCalls     int
	checked       []int
	occupied      map[int]bool
	beforeConfirm func(context.Context) error
}

func (p *autoCapacityPVE) NextID(ctx context.Context) (int, error) {
	if _, ok := ctx.Deadline(); !ok {
		return 0, errors.New("unbounded VMID selection")
	}
	p.nextCalls++
	return p.suggestion, nil
}
func (p *autoCapacityPVE) ConfirmAbsent(ctx context.Context, id int) error {
	p.checked = append(p.checked, id)
	if p.beforeConfirm != nil {
		if err := p.beforeConfirm(ctx); err != nil {
			return err
		}
	}
	if p.occupied[id] {
		return proxmox.ErrCollision
	}
	return nil
}
func autoCapacityFixture(t *testing.T, automaticEphemeral ...bool) (*dispatchServer, store.EphemeralChildIntent, *autoCapacityPVE) {
	t.Helper()
	d, intent, _ := ephemeralCapacityFixture(t, automaticEphemeral...)
	pool, err := d.srv.store.GetManagedPool(context.Background(), intent.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := d.srv.managed.registry.pools[pool.Connection].runtime
	runtime.actors = pool.MaxActors
	runtime.cpuMillicores = pool.Resources.CPUMillicores
	runtime.memoryBytes = pool.Resources.MemoryBytes
	runtime.diskGiB = pool.Resources.DiskBytes / gib
	p := &autoCapacityPVE{managedFakePVE: &managedFakePVE{server: d.srv}, suggestion: d.master.CTID, occupied: map[int]bool{}}
	d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return p, nil }
	return d, intent, p
}

// FEATURE: Автоматический allocator выбирает только новые metadata, не запускает CT.
// SPEC: Project/pool admission и FULL scope recheck, VMID вне всех cluster holds;
// shortages оставляют job в FIFO, повтор возвращает исходную allocation без effects.
// IMPLEMENTATION: Общий project gate и FULL allocation после read-only VMID scan.
// RESULT: IMPLEMENTED — полный Go/Python прогон и focused race проходят.
// STATUS: DONE
func TestAutoEphemeralAdmissionSelectsAndRetainsOneAllocation(t *testing.T) {
	ctx := context.Background()
	d, intent, p := autoCapacityFixture(t)
	actors, err := d.srv.store.ListManagedActors(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	held := map[int]bool{}
	for _, a := range actors {
		held[a.CTID] = true
	}
	first := p.suggestion
	for held[first] {
		first++
	}
	p.occupied[first] = true
	want := first + 1
	for held[want] {
		want++
	}
	createSQL(t, d, `CREATE TRIGGER auto_admission_full BEFORE INSERT ON managed_actors WHEN NEW.actor_class='ephemeral' AND (SELECT synchronous FROM pragma_synchronous)<>2 BEGIN SELECT RAISE(ABORT,'automatic admission requires FULL'); END`)
	p.beforeConfirm = func(context.Context) error {
		createSQL(t, d, `UPDATE users SET username=username WHERE id=?`, d.master.DelegatorID)
		return nil
	}
	allocation, hint, err := d.srv.allocateQueuedEphemeralChild(ctx, intent.ID, 0, 4)
	if err != nil || hint != 0 {
		t.Fatalf("automatic admission: %+v hint=%d %v", allocation, hint, err)
	}
	actor, err := d.srv.store.GetManagedActor(ctx, allocation.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := d.srv.store.GetManagedPool(ctx, intent.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	resources, err := managedActorResources(pool)
	if err != nil {
		t.Fatal(err)
	}
	if actor.CTID != want || actor.UID == d.master.UID || actor.UID == d.worker.UID || actor.OwnerNonce == "" || actor.OperationID == "" || actor.Class != store.ActorClassEphemeral || actor.ParentUID != d.master.UID || actor.Resources != resources || actor.Stage != "create" || actor.UPID != "" {
		t.Fatalf("allocator invented wrong actor or effect: %+v", actor)
	}
	if !reflect.DeepEqual(p.checked, []int{first, want}) {
		t.Fatalf("selector probed reserved IDs: %v", p.checked)
	}
	grant, err := d.srv.store.GetEphemeralChildCreateGrant(ctx, intent.ID)
	if err != nil || grant.State != "queued" {
		t.Fatalf("allocation issued Create: %+v %v", grant, err)
	}
	beforeNext, beforeChecks := p.nextCalls, len(p.checked)
	again, _, err := d.srv.allocateQueuedEphemeralChild(ctx, intent.ID, 0, 4)
	if err != nil || again != allocation || p.nextCalls != beforeNext || len(p.checked) != beforeChecks {
		t.Fatalf("retry selected another reservation: %+v %v", again, err)
	}
	if effects := providerEffectsOf(p.managedFakePVE); effects.creates != 0 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("admission wrote to provider: %+v", effects)
	}
}

func TestAutoEphemeralAdmissionHoldsQueueOnCapacityOrScopeFailure(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"full-pool", "full-project", "draining", "revoked", "changed-after-selection", "pool-after-selection", "namespace-after-selection", "scan-budget"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, p := autoCapacityFixture(t)
			hint, budget := 0, 4
			switch scenario {
			case "full-pool":
				pool, err := d.srv.store.GetManagedPool(ctx, intent.PoolUID)
				if err != nil {
					t.Fatal(err)
				}
				pool.MaxActors = 2
				if _, err := d.srv.store.UpdateManagedPool(ctx, pool, pool.Revision); err != nil {
					t.Fatal(err)
				}
				d.srv.managed.registry.pools[pool.Connection].runtime.actors = pool.MaxActors
			case "full-project":
				zero := int64(0)
				if err := d.srv.store.SetProjectCapacity(ctx, d.projectID(t), store.ProjectCapacity{MaxActors: &zero}); err != nil {
					t.Fatal(err)
				}
			case "draining":
				pool, err := d.srv.store.GetManagedPool(ctx, intent.PoolUID)
				if err != nil {
					t.Fatal(err)
				}
				pool.State = "draining"
				if _, err := d.srv.store.UpdateManagedPool(ctx, pool, pool.Revision); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if err := d.srv.store.SetUserAdmin(ctx, d.master.DelegatorID, false); err != nil {
					t.Fatal(err)
				}
			case "changed-after-selection":
				p.beforeConfirm = func(context.Context) error { return d.srv.store.SetUserAdmin(ctx, d.master.DelegatorID, false) }
			case "pool-after-selection":
				p.beforeConfirm = func(ctx context.Context) error {
					pool, err := d.srv.store.GetManagedPool(ctx, intent.PoolUID)
					if err != nil {
						return err
					}
					pool.Labels["changed"] = "true"
					_, err = d.srv.store.UpdateManagedPool(ctx, pool, pool.Revision)
					return err
				}
			case "namespace-after-selection":
				p.beforeConfirm = func(ctx context.Context) error {
					swarm, err := d.srv.store.GetSwarm(ctx, d.swarm)
					if err != nil {
						return err
					}
					d.fake.mu.Lock()
					d.fake.atespaces[swarm.AtespaceName].Metadata.Uid = "replacement"
					d.fake.mu.Unlock()
					return nil
				}
			case "scan-budget":
				hint, budget = 900, 1
				p.occupied[900] = true
			}
			_, next, err := d.srv.allocateQueuedEphemeralChild(ctx, intent.ID, hint, budget)
			if err == nil {
				t.Fatal("unavailable admission accepted")
			}
			if (scenario == "full-pool" || scenario == "full-project" || scenario == "draining" || scenario == "revoked") && (p.nextCalls != 0 || len(p.checked) != 0) {
				t.Fatal("known capacity or authority refusal queried provider IDs")
			}
			if scenario == "scan-budget" && next != 901 {
				t.Fatalf("lost read-only scan cursor: %d", next)
			}
			if _, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("denial allocated actor: %v", err)
			}
			queued, err := d.srv.store.QueuedEphemeralChildIntents(ctx, 0)
			if err != nil || len(queued) != 1 || queued[0].ID != intent.ID {
				t.Fatalf("denial failed or dropped queued work: %+v %v", queued, err)
			}
			if _, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("capacity or scope invented failure: %v", err)
			}
			if effects := providerEffectsOf(p.managedFakePVE); effects.creates != 0 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
				t.Fatalf("failed admission sent work: %+v", effects)
			}
		})
	}
}

// BUG: Повтор allocation обходил текущую привязку роли и состояние parent step.
// HYPOTHESIS: Ветка retained allocation вызывала только callback, а не Store gate.
// SOLUTION: Повтор проходит тот же FULL Store gate с исходным reservation.
// RESULT: FIXED — смена роли и остановка роя запрещают повтор без потери binding.
// STATUS: FIXED
func TestAutoEphemeralAdmissionRetryRechecksParentStep(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"role-rebound", "stopped"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, p := autoCapacityFixture(t)
			allocation, _, err := d.srv.allocateQueuedEphemeralChild(ctx, intent.ID, 0, 4)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "role-rebound" {
				createSQL(t, d, `UPDATE swarm_role_bindings SET actor_uid=?,actor_incarnation=? WHERE swarm_id=? AND actor_uid=?`, d.worker.UID, d.worker.Incarnation, d.swarm, d.master.UID)
			} else {
				d.stop()
			}
			beforeNext, beforeChecks := p.nextCalls, len(p.checked)
			if got, _, err := d.srv.allocateQueuedEphemeralChild(ctx, intent.ID, 0, 4); err == nil {
				t.Fatalf("retained allocation bypassed revoked parent-step authority: %+v", got)
			}
			got, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID)
			if err != nil || got != allocation || p.nextCalls != beforeNext || len(p.checked) != beforeChecks {
				t.Fatalf("refusal changed allocation or selected another VMID: %+v %v", got, err)
			}
			if effects := providerEffectsOf(p.managedFakePVE); effects.creates != 0 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
				t.Fatalf("retry sent provider effects: %+v", effects)
			}
		})
	}
}

func TestAutoEphemeralAdmissionDoesNotReuseUnissuedReservation(t *testing.T) {
	ctx := context.Background()
	d, intent, p := autoCapacityFixture(t)
	second, err := d.srv.store.EnqueueEphemeralChild(ctx, intent.Parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "second-auto", "Another task", intent.PoolUID, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := d.srv.allocateQueuedEphemeralChild(ctx, intent.ID, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := d.srv.allocateQueuedEphemeralChild(ctx, second.ID, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	a, err := d.srv.store.GetManagedActor(ctx, first.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.srv.store.GetManagedActor(ctx, next.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	if a.UID == b.UID || a.CTID == b.CTID || a.OwnerNonce == b.OwnerNonce || a.OperationID == b.OperationID {
		t.Fatal("separate jobs reused held identity or CTID")
	}
	if effects := providerEffectsOf(p.managedFakePVE); effects.creates != 0 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("queued reservations caused effects: %+v", effects)
	}
}
