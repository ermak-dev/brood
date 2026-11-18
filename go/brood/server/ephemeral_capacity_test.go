package server

// FEATURE: Резервирование метаданных эфемерного актора под блокировкой проекта (#292).
// SPEC: Учитывать оба вида акторов, перепроверять владельца и родителя,
// сохранять очередь при отказе и не запускать worker.
// IMPLEMENTATION: Сетевой снимок снимается до durable-транзакции;
// права, селектор и managed-инвентарь перепроверяются через её ctx.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

func ephemeralCapacityFixture(t *testing.T, automaticEphemeral ...bool) (*dispatchServer, store.EphemeralChildIntent, store.EphemeralActorReservation) {
	t.Helper()
	d := newChildEngineServer(t, automaticEphemeral...)
	ctx := context.Background()
	p, err := d.srv.store.GetManagedPool(ctx, d.master.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	p.MaxActors = 20
	p.Resources.CPUMillicores *= 20
	p.Resources.MemoryBytes *= 20
	p.Resources.DiskBytes *= 20
	if _, err = d.srv.store.UpdateManagedPool(ctx, p, p.Revision); err != nil {
		t.Fatal(err)
	}
	parent := store.SwarmChildParent{Principal: childPrincipal(d.master, d.projectID(t)), Pass: 1, Sequence: 1, RequestID: d.committed().Request}
	intent, err := d.srv.store.EnqueueEphemeralChild(ctx, parent, store.SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "isolated", "One isolated task", p.UID, d.srv.managed.childAuthority)
	if err != nil {
		t.Fatal(err)
	}
	// Любой вызов провайдера после подготовки фикстуры запрещён.
	d.srv.managed.clients = map[string]proxmoxAPI{}
	d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
		t.Error("metadata adapter reached provider")
		return nil, errors.New("provider forbidden")
	}
	return d, intent, store.EphemeralActorReservation{UID: "isolated-worker", OwnerNonce: "isolated-nonce", OperationID: "isolated-create", CTID: 909, Resources: d.master.Resources}
}

func (d *dispatchServer) projectID(t *testing.T) string {
	t.Helper()
	sw, err := d.srv.store.GetSwarm(context.Background(), d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	return sw.ProjectID
}

type ephemeralGateHold struct {
	entered chan struct{}
	release chan struct{}
}

func (h ephemeralGateHold) Prepare([]app.Swarm) (app.WorkloadAdmission, error) {
	close(h.entered)
	<-h.release
	return app.WorkloadAdmission{}, app.ErrConflict
}
func (h ephemeralGateHold) Apply(map[string]string) error {
	return errors.New("gate hold must not apply")
}

func TestEphemeralCapacityUsesSharedProjectGate(t *testing.T) {
	d, intent, reservation := ephemeralCapacityFixture(t)
	ctx := context.Background()
	hold := ephemeralGateHold{entered: make(chan struct{}), release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(hold.release)
		}
	}()
	held := make(chan error, 1)
	go func() { held <- d.srv.app.AdmitWorkload(d.master.DelegatorID, d.swarm, hold) }()
	select {
	case <-hold.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("shared gate holder did not prepare")
	}
	inspected := make(chan struct{}, 1)
	d.fake.mu.Lock()
	d.fake.beforeResourceRead = func() {
		select {
		case inspected <- struct{}{}:
		default:
		}
	}
	d.fake.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, reservation)
		done <- err
	}()
	early := false
	select {
	case <-inspected:
		early = true
	case <-time.After(100 * time.Millisecond):
	}
	close(hold.release)
	released = true
	if err := <-held; !errors.Is(err, app.ErrConflict) {
		t.Fatalf("gate holder: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("allocation did not resume after gate release")
	}
	if early {
		t.Fatal("ephemeral native inspection escaped the shared project gate")
	}
}

func TestEphemeralCapacityRechecksAfterNativeRead(t *testing.T) {
	for _, scenario := range []string{"owner", "delegation", "parent-role"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, reservation := ephemeralCapacityFixture(t)
			ctx := context.Background()
			root, err := d.srv.store.GetUserByUsername(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			before, err := d.srv.store.ListManagedActors(ctx, d.swarm)
			if err != nil {
				t.Fatal(err)
			}
			var hookErr error
			called := false
			d.fake.mu.Lock()
			d.fake.beforeResourceRead = func() {
				if called {
					return
				}
				called = true
				// Запись во время native RPC завершится только вне Store transaction.
				switch scenario {
				case "owner":
					hookErr = d.srv.store.SetUserDisabled(ctx, root.ID, true)
				case "delegation":
					hookErr = d.srv.store.SetUserAdmin(ctx, d.master.DelegatorID, false)
				case "parent-role":
					hookErr = d.srv.store.BindSwarmRole(ctx, store.SwarmRoleBinding{SwarmID: d.swarm, Role: "build", ActorUID: d.worker.UID})
				}
			}
			d.fake.mu.Unlock()
			got, err := d.srv.allocateEphemeralChild(ctx, root.ID, intent.Parent, intent.ID, reservation)
			d.fake.mu.Lock()
			d.fake.beforeResourceRead = nil
			d.fake.mu.Unlock()
			if !called || hookErr != nil {
				t.Fatalf("revocation hook: called=%v err=%v", called, hookErr)
			}
			if err == nil || got != (store.EphemeralChildAllocation{}) {
				t.Fatalf("revoked authority admitted: %+v %v", got, err)
			}
			after, readErr := d.srv.store.ListManagedActors(ctx, d.swarm)
			if readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("revocation changed inventory: %v", readErr)
			}
			if _, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("partial binding: %v", err)
			}
		})
	}
}

func TestEphemeralCapacitySharedAdmission(t *testing.T) {
	for _, scenario := range []string{"success", "lowered-swarm-limit", "actors", "cpu", "memory", "zero", "selector", "nil-native", "bad-native", "owner", "parent", "zero-resource"} {
		t.Run(scenario, func(t *testing.T) {
			d, intent, reservation := ephemeralCapacityFixture(t)
			ctx := context.Background()
			sw, err := d.srv.store.GetSwarm(ctx, d.swarm)
			if err != nil {
				t.Fatal(err)
			}
			// Третий резерв native обязателен: одного managed-инвентаря недостаточно.
			template := inventoryTemplate(sw.AtespaceName, "native-base", "native-template", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
			d.fake.mu.Lock()
			d.fake.templates[sw.AtespaceName+"/native-base"] = template
			d.fake.actors[sw.AtespaceName+"/native"] = &ateapi.Actor{Metadata: fakeMeta("native", sw.AtespaceName), ActorTemplate: &ateapi.ObjectRef{Name: "native-base", Atespace: sw.AtespaceName}}
			d.fake.mu.Unlock()
			three := int64(3)
			cpu := d.master.Resources.CPUMillicores + d.worker.Resources.CPUMillicores + template.GetResources().GetLimits().GetCpuMillicores()
			memory := d.master.Resources.MemoryBytes + d.worker.Resources.MemoryBytes + template.GetResources().GetLimits().GetMemoryBytes()
			cap := store.ProjectCapacity{}
			switch scenario {
			case "lowered-swarm-limit":
				zero := int64(0)
				cap.MaxSwarms = &zero
			case "actors":
				cap.MaxActors = &three
			case "cpu":
				cap.MaxCPUMillicores = &cpu
			case "memory":
				cap.MaxMemoryBytes = &memory
			case "zero":
				zero := int64(0)
				cap.MaxActors = &zero
			case "selector":
				cap.WorkerSelector = map[string]string{"missing-pool-label": "required"}
			case "nil-native":
				d.srv.ateapi = nil
			case "bad-native":
				d.fake.mu.Lock()
				delete(d.fake.templates, sw.AtespaceName+"/native-base")
				d.fake.mu.Unlock()
			case "owner":
				if err = d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true); err != nil {
					t.Fatal(err)
				}
			case "parent":
				intent.Parent.Principal.Incarnation++
			case "zero-resource":
				reservation.Resources.CPUMillicores = 0
			}
			if err := d.srv.store.SetProjectCapacity(ctx, sw.ProjectID, cap); err != nil {
				t.Fatal(err)
			}
			before, err := d.srv.store.ListManagedActors(ctx, d.swarm)
			if err != nil {
				t.Fatal(err)
			}
			d.mu.Lock()
			messages := len(d.messages)
			attests := len(d.attests)
			d.mu.Unlock()
			got, err := d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, reservation)
			if scenario == "success" || scenario == "lowered-swarm-limit" {
				if err != nil {
					t.Fatal(err)
				}
				a, err := d.srv.store.GetManagedActor(ctx, got.ActorUID)
				if err != nil || a.Class != store.ActorClassEphemeral || a.Incarnation != 1 || a.Stage != "create" || a.OperationID != reservation.OperationID || a.ParentUID != d.master.UID {
					t.Fatalf("not a durable metadata reservation: %+v %v", a, err)
				}
				join, err := d.srv.store.GetSwarmChildJoin(ctx, intent.Parent, d.srv.managed.childAuthority)
				if err != nil || len(join.Children) != 0 {
					t.Fatalf("allocation registered child execution: %+v %v", join, err)
				}
				// Повтор на точном полном лимите не добавляет ещё одного резерва.
				four := int64(4)
				if err := d.srv.store.SetProjectCapacity(ctx, sw.ProjectID, store.ProjectCapacity{MaxActors: &four}); err != nil {
					t.Fatal(err)
				}
				retry, err := d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, reservation)
				if err != nil || retry != got {
					t.Fatalf("exact retry: %+v %v", retry, err)
				}
				changed := reservation
				changed.OperationID = "different"
				if _, err := d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, changed); !errors.Is(err, store.ErrSwarmChildConflict) {
					t.Fatalf("changed retry: %v", err)
				}
				if err := d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true); err != nil {
					t.Fatal(err)
				}
				if _, err := d.srv.allocateEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, reservation); err == nil {
					t.Fatal("revoked retry accepted")
				}
			} else {
				if err == nil || got != (store.EphemeralChildAllocation{}) {
					t.Fatalf("refusal lost: %+v %v", got, err)
				}
				if scenario == "actors" || scenario == "cpu" || scenario == "memory" || scenario == "zero" {
					var capacity *app.CapacityExceededError
					if !errors.As(err, &capacity) {
						t.Fatalf("wrong capacity refusal: %v", err)
					}
				}
				after, err := d.srv.store.ListManagedActors(ctx, d.swarm)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("refusal wrote partial worker metadata")
				}
				queued, err := d.srv.store.QueuedEphemeralChildIntents(ctx, 0)
				if err != nil || len(queued) != 1 || queued[0].ID != intent.ID {
					t.Fatalf("queue lost: %+v %v", queued, err)
				}
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if len(d.messages) != messages || len(d.attests) != attests {
				t.Fatal("allocation activated guest work")
			}
			d.fake.mu.Lock()
			defer d.fake.mu.Unlock()
			if len(d.fake.actors) != 1 {
				t.Fatal("allocation mutated native actors")
			}
		})
	}
}
