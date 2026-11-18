package server

// FEATURE: Приватная физическая отправка только по свежему create grant (#292).
// SPEC: Один Create на исходный allocation; durable grant до эффекта; owner gates
// родителя и исполнителя до последней локальной проверки и до возврата Create.
// IMPLEMENTATION: Namespace/client preflight вне tx, текущие owner/scheduling
// права внутри grant callback; оба effect gate удерживаются через Create/recordTask.
// RESULT: IMPLEMENTED — focused/race, полный Go build/test/vet прошли.
// STATUS: DONE

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type ephemeralCreatePVE struct {
	*managedFakePVE
	calls  atomic.Int32
	before func(context.Context, proxmox.CTSpec) error
	after  func()
}

func (p *ephemeralCreatePVE) Create(ctx context.Context, spec proxmox.CTSpec) (string, error) {
	p.calls.Add(1)
	if p.before != nil {
		if err := p.before(ctx, spec); err != nil {
			return "", err
		}
	}
	upid, err := p.managedFakePVE.Create(ctx, spec)
	if p.after != nil {
		p.after()
	}
	return upid, err
}
func ephemeralCreateFixture(t *testing.T) (*dispatchServer, store.EphemeralChildIntent, store.ManagedActor, *ephemeralCreatePVE) {
	t.Helper()
	d, intent, r := ephemeralCapacityFixture(t)
	allocation, err := d.srv.allocateEphemeralChild(context.Background(), d.master.DelegatorID, intent.Parent, intent.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	a, err := d.srv.store.GetManagedActor(context.Background(), allocation.ActorUID)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := d.srv.store.GetManagedPool(context.Background(), a.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := d.srv.managed.registry.pools[pool.Connection].runtime
	runtime.actors = pool.MaxActors
	runtime.cpuMillicores = pool.Resources.CPUMillicores
	runtime.memoryBytes = pool.Resources.MemoryBytes
	runtime.diskGiB = pool.Resources.DiskBytes / gib
	p := &ephemeralCreatePVE{managedFakePVE: &managedFakePVE{server: d.srv}}
	d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return p, nil }
	return d, intent, a, p
}
func createSQL(t *testing.T, d *dispatchServer, q string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", d.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
func requireCreateHold(t *testing.T, d *dispatchServer, i store.EphemeralChildIntent, a store.ManagedActor) {
	t.Helper()
	g, err := d.srv.store.GetEphemeralChildCreateGrant(context.Background(), i.ID)
	if err != nil || g.State != "reserved" || !g.Replay || g.ActorUID != a.UID || g.CreateOperationID != a.OperationID {
		t.Fatalf("grant hold: %+v %v", g, err)
	}
	current, err := d.srv.store.GetManagedActor(context.Background(), a.UID)
	if err != nil || current.UID != a.UID || current.CTID != a.CTID || current.OwnerNonce != a.OwnerNonce || current.OperationID != a.OperationID || current.Incarnation != a.Incarnation {
		t.Fatalf("inventory changed: %+v %v", current, err)
	}
}
func TestEphemeralCreateFreshCommitSpecAndTask(t *testing.T) {
	d, i, a, p := ephemeralCreateFixture(t)
	p.before = func(ctx context.Context, spec proxmox.CTSpec) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded provider context")
		}
		db, err := sql.Open("sqlite", d.dbPath)
		if err != nil {
			return err
		}
		defer db.Close()
		var state, reserved string
		if err = db.QueryRow(`SELECT state,reserved_at FROM ephemeral_child_create_grants WHERE intent_id=?`, i.ID).Scan(&state, &reserved); err != nil {
			return err
		}
		if state != "reserved" || reserved == "" {
			t.Fatalf("effect preceded committed grant: %s %s", state, reserved)
		}
		// Независимое соединение может писать: сеть не находится в Store callback.
		if _, err = db.Exec(`UPDATE users SET username=username WHERE id=?`, d.master.DelegatorID); err != nil {
			return err
		}
		if spec.VMID != a.CTID || spec.Owner.ActorUID != a.UID || spec.Owner.PoolUID != a.PoolUID || spec.Owner.Nonce != a.OwnerNonce || spec.Owner.Incarnation != 1 || spec.Template != a.Template.URI {
			t.Fatalf("foreign physical spec: %+v", spec)
		}
		if a.Origin != i.Origin || spec.Env["BROOD_GUEST_ACTOR_UID"] != a.UID || spec.Env["BROOD_GUEST_GENERATION"] != "1" || spec.Env["BROOD_GUEST_RELAY_TOKEN"] == "" || !strings.Contains(spec.Env["BROOD_GUEST_RELAY_URL"], "/"+a.UID+"/1") || spec.Env["BROOD_GUEST_RELAY_HOST_KEY"] == "" {
			t.Fatalf("bootstrap identity missing: %+v", spec.Env)
		}
		for k, v := range spec.Env {
			if strings.Contains(strings.ToLower(k), "task") || strings.Contains(v, i.Payload) {
				t.Fatalf("task disclosed in bootstrap %s", k)
			}
		}
		return nil
	}
	got, err := d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
	if err != nil || got.UPID != "create-task" || got.OperationID != a.OperationID || got.Stage != "create" {
		t.Fatalf("create task: %+v %v", got, err)
	}
	persisted, err := d.srv.store.GetManagedActor(context.Background(), a.UID)
	if err != nil || persisted.UPID != got.UPID || persisted.OperationID != a.OperationID {
		t.Fatalf("task not bound: %+v %v", persisted, err)
	}
	for range 2 {
		_, _ = d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
	}
	requireCreateHold(t, d, i, a)
	if p.creates != 1 || p.starts != 0 || p.stops != 0 || p.deletes != 0 {
		t.Fatalf("unexpected effects: %+v", providerEffectsOf(p.managedFakePVE))
	}
}
func TestEphemeralCreateAllowsCurrentNonAdminOwner(t *testing.T) {
	d, i, _, p := ephemeralCreateFixture(t)
	sw, err := d.srv.store.GetSwarm(context.Background(), d.swarm)
	if err != nil {
		t.Fatal(err)
	}
	root, err := d.srv.store.GetUserByUsername(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	// Parent scheduling по-прежнему одобрено действующим admin; человек,
	// отправляющий private create, имеет только обычные права owner.
	createSQL(t, d, `UPDATE managed_actors SET delegator_id=? WHERE uid=?`, root.ID, d.master.UID)
	if err := d.srv.store.SetUserAdmin(context.Background(), sw.OwnerID, false); err != nil {
		t.Fatal(err)
	}
	user, err := d.srv.store.GetUser(context.Background(), sw.OwnerID)
	if err != nil || user.IsAdmin {
		t.Fatal("fixture owner must not be admin")
	}
	if _, err := d.srv.createAllocatedEphemeralChild(context.Background(), user.ID, i.Parent, i.ID); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 1 {
		t.Fatal("owner create was not issued")
	}
}

func TestEphemeralCreateConcurrentExact(t *testing.T) {
	d, i, a, p := ephemeralCreateFixture(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, _ = d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
		})
	}
	wg.Wait()
	if p.creates != 1 {
		t.Fatalf("Create count %d", p.creates)
	}
	requireCreateHold(t, d, i, a)
}
func TestEphemeralCreateUnknownAndBookkeepingNeverReissue(t *testing.T) {
	for _, scenario := range []string{"lost-reply", "error-with-upid", "record-task", "known-unsent", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			d, i, a, p := ephemeralCreateFixture(t)
			switch scenario {
			case "lost-reply", "restart":
				p.uncertain = true
			case "error-with-upid":
				p.uncertainWithUPID = true
			case "record-task":
				p.after = func() {
					createSQL(t, d, `CREATE TRIGGER refuse_create_task BEFORE UPDATE OF upid ON managed_actors BEGIN SELECT RAISE(ABORT,'test bookkeeping failure'); END`)
				}
			case "known-unsent":
				p.before = func(context.Context, proxmox.CTSpec) error { return proxmox.ErrPermission }
			}
			_, firstErr := d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
			if scenario == "record-task" && firstErr == nil {
				t.Fatal("bookkeeping failure hidden")
			}
			current, readErr := d.srv.store.GetManagedActor(context.Background(), a.UID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "error-with-upid" {
				if current.UPID != "create-task" || current.Phase != "error" {
					t.Fatalf("lost task evidence: %+v", current)
				}
			} else if current.UPID != "" {
				t.Fatal("invented provider task")
			}
			requireCreateHold(t, d, i, a)
			if scenario == "restart" {
				reopened, err := store.Open(context.Background(), d.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				d.srv.actorDelivery.close()
				old := d.srv.store
				d.srv.store = reopened
				defer func() { d.srv.store = old }()
			}
			for range 2 {
				_, _ = d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
			}
			want := 1
			if scenario == "known-unsent" {
				want = 0
			}
			if p.creates != want || p.calls.Load() != 1 {
				t.Fatalf("Create repeated: physical=%d invocations=%d", p.creates, p.calls.Load())
			}
			requireCreateHold(t, d, i, a)
		})
	}
}
func TestEphemeralCreateRefusals(t *testing.T) {
	for _, scenario := range []string{"empty-user", "foreign-user", "wrong-parent", "empty-intent", "persistent", "unbound", "foreign-parent", "incarnation", "operation", "nonce", "owner", "delegator", "pool", "template", "source", "parent-inc", "reserved"} {
		t.Run(scenario, func(t *testing.T) {
			d, i, a, p := ephemeralCreateFixture(t)
			ctx := context.Background()
			user := d.master.DelegatorID
			switch scenario {
			case "empty-user":
				user = ""
			case "foreign-user":
				u, e := d.srv.store.GetUserByUsername(ctx, "bob")
				if e != nil {
					t.Fatal(e)
				}
				user = u.ID
			case "wrong-parent":
				i.Parent.Sequence++
			case "empty-intent":
				i.ID = ""
			case "persistent":
				// Имитация чужой/повреждённой привязки; обычный Store запрещает смену класса.
				createSQL(t, d, `DROP TRIGGER managed_actor_class_update_guard`)
				createSQL(t, d, `UPDATE managed_actors SET actor_class='persistent' WHERE uid=?`, a.UID)
			case "unbound":
				i.ID = "missing-intent"
			case "foreign-parent":
				createSQL(t, d, `UPDATE managed_actors SET parent_uid=? WHERE uid=?`, d.worker.UID, a.UID)
			case "incarnation":
				createSQL(t, d, `UPDATE managed_actors SET incarnation=2 WHERE uid=?`, a.UID)
			case "operation":
				createSQL(t, d, `UPDATE managed_actors SET operation_id='different' WHERE uid=?`, a.UID)
			case "nonce":
				createSQL(t, d, `UPDATE managed_actors SET owner_nonce='different' WHERE uid=?`, a.UID)
			case "owner":
				if e := d.srv.store.SetUserDisabled(ctx, user, true); e != nil {
					t.Fatal(e)
				}
			case "delegator":
				if e := d.srv.store.SetUserAdmin(ctx, d.master.DelegatorID, false); e != nil {
					t.Fatal(e)
				}
			case "pool":
				createSQL(t, d, `UPDATE managed_pools SET state='draining' WHERE uid=?`, a.PoolUID)
			case "template":
				createSQL(t, d, `UPDATE managed_actors SET source_fingerprint='different' WHERE uid=?`, a.UID)
			case "source":
				if e := d.srv.store.SetProjectRepository(ctx, d.projectID(t), "https://github.com/other/repo.git"); e != nil {
					t.Fatal(e)
				}
			case "parent-inc":
				createSQL(t, d, `UPDATE managed_actors SET incarnation=incarnation+1 WHERE uid=?`, d.master.UID)
			case "reserved":
				if _, e := d.srv.store.ReserveEphemeralChildCreate(ctx, i.ID, d.srv.managed.childAuthority); e != nil {
					t.Fatal(e)
				}
			}
			_, err := d.srv.createAllocatedEphemeralChild(ctx, user, i.Parent, i.ID)
			if err == nil && scenario != "reserved" {
				t.Fatal("invalid authority succeeded")
			}
			if p.creates != 0 {
				t.Fatal("refusal reached Create")
			}
		})
	}
}
func createOwnerIntervention(t *testing.T, d *dispatchServer, a store.ManagedActor) error {
	t.Helper()
	if a.Phase == "provisioning" {
		// Session control закономерно требует ready. Для provisioning используем
		// существующий durable owner lifecycle boundary с настоящими owner rights.
		ctx := context.Background()
		release, err := d.srv.managed.effects.acquire(ctx, a.UID)
		if err != nil {
			return err
		}
		defer release()
		op, err := d.srv.store.AcquireOwnerActorOperation(ctx, a, store.NewID(), "create-owner", "delete", d.master.DelegatorID, func(ctx context.Context) error {
			return d.srv.authorizeSwarmOwnerIntent(ctx, d.master.DelegatorID, d.swarm)
		})
		if err != nil {
			return err
		}
		return d.srv.store.FinishActorOperation(ctx, op, true)
	}
	// Обычный owner session path: реальная авторизация, gate и durable intervention.
	base := d.srv.managed.hub.(commandSessionHub)
	base.call = func(context.Context, string, int64, relay.SessionRequest) (relay.SessionResponse, error) {
		return relay.SessionResponse{OK: true, Outcome: "complete", Text: "ok"}, nil
	}
	d.srv.managed.hub = connectHub{commandSessionHub: base, connected: func(commandSessionHub, string, int64) bool { return true }}
	_, err := d.srv.executeOperatorSessionCommand(context.Background(), d.master.DelegatorID, d.swarm, "create-owner", command.Invocation{Name: "restart", Arguments: []byte(`{"name":"` + a.Name + `"}`)})
	return err
}
func TestEphemeralCreatePreflightBeforeBothOwnerGates(t *testing.T) {
	for _, target := range []string{"parent", "executor"} {
		t.Run(target, func(t *testing.T) {
			d, i, a, p := ephemeralCreateFixture(t)
			entered, resume := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-resume:
				default:
					close(resume)
				}
			}()
			d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { close(entered); <-resume; return p, nil }
			done := make(chan error, 1)
			go func() {
				_, e := d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
				done <- e
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("no preflight")
			}
			actor := a
			if target == "parent" {
				actor = d.master
			}
			if err := createOwnerIntervention(t, d, actor); err != nil {
				t.Fatal(err)
			}
			close(resume)
			select {
			case e := <-done:
				if e == nil {
					t.Fatal("intervention ignored")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("gate leak")
			}
			if p.creates != 0 {
				t.Fatal("Create after completed owner intervention")
			}
		})
	}
}
func TestEphemeralCreateHoldsBothGatesThroughProviderWrapper(t *testing.T) {
	for _, target := range []string{"parent", "executor"} {
		t.Run(target, func(t *testing.T) {
			d, i, a, p := ephemeralCreateFixture(t)
			actor := a
			if target == "parent" {
				actor = d.master
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-resume:
				default:
					close(resume)
				}
			}()
			p.before = func(ctx context.Context, _ proxmox.CTSpec) error {
				close(entered)
				select {
				case <-resume:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			created := make(chan error, 1)
			go func() {
				_, err := d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
				created <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("no provider wrapper")
			}
			ownerDone := make(chan error, 1)
			go func() { ownerDone <- createOwnerIntervention(t, d, actor) }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				gates := &d.srv.managed.effects
				gates.mu.Lock()
				entry := gates.entries[actor.UID]
				blocked := entry != nil && entry.users >= 2
				gates.mu.Unlock()
				if blocked {
					break
				}
				select {
				case err := <-ownerDone:
					t.Fatalf("owner completed inside delayed Create: %v", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("owner did not reach shared gate")
				}
				runtime.Gosched()
			}
			// Проверяем независимое persisted evidence до разрешения настоящего Create.
			db, err := sql.Open("sqlite", d.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			var interventions int
			err = db.QueryRow(`SELECT count(*) FROM actor_interventions WHERE actor_uid=?`, actor.UID).Scan(&interventions)
			db.Close()
			if err != nil || interventions != 0 {
				t.Fatalf("owner committed ahead of physical issue: %d %v", interventions, err)
			}
			close(resume)
			select {
			case err := <-created:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("create gate leaked")
			}
			select {
			case err := <-ownerDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("owner gate leaked")
			}
			if p.calls.Load() != 1 || p.creates != 1 {
				t.Fatal("physical issue missing or repeated")
			}
			_, _ = d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
			if p.calls.Load() != 1 {
				t.Fatal("post-intervention retry reissued Create")
			}
		})
	}
}

func TestEphemeralCreateRefusesParentUnknownWorkspace(t *testing.T) {
	d, i, _, p := ephemeralCreateFixture(t)
	ctx := context.Background()
	// Старый workspace hold мог предшествовать metadata allocation. Новый
	// AcquireActorOperation уже закрыт child obligations, поэтому seed истории.
	op := store.ActorOperation{ProjectID: i.Parent.Principal.ProjectID, SwarmID: d.swarm, UID: d.master.UID, Generation: d.master.Incarnation, OperationID: "parent-unknown", Caller: "trusted", Operation: "restart", State: "unconfirmed"}
	payload, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	createSQL(t, d, `INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, string(payload), op.UID)
	_, err = d.srv.createAllocatedEphemeralChild(ctx, d.master.DelegatorID, i.Parent, i.ID)
	if err == nil || p.calls.Load() != 0 {
		t.Fatalf("parent unknown effect admitted Create: %v calls=%d", err, p.calls.Load())
	}
}

func TestEphemeralCreateRechecksAuthorityAfterPreflight(t *testing.T) {
	for _, scenario := range []string{"owner", "pool", "operation", "source", "namespace"} {
		t.Run(scenario, func(t *testing.T) {
			d, i, a, p := ephemeralCreateFixture(t)
			changed := false
			mutate := func() {
				changed = true
				switch scenario {
				case "owner":
					if err := d.srv.store.SetUserDisabled(context.Background(), d.master.DelegatorID, true); err != nil {
						t.Error(err)
					}
				case "pool":
					createSQL(t, d, `UPDATE managed_pools SET state='draining' WHERE uid=?`, a.PoolUID)
				case "operation":
					createSQL(t, d, `UPDATE managed_actors SET operation_id='replacement' WHERE uid=?`, a.UID)
				case "source":
					if err := d.srv.store.SetProjectRepository(context.Background(), i.Parent.Principal.ProjectID, "https://github.com/other/repo.git"); err != nil {
						t.Error(err)
					}
				case "namespace":
					createSQL(t, d, `UPDATE swarms SET atespace_uid='replacement' WHERE id=?`, d.swarm)
				}
			}
			// Запись с независимого соединения в сетевом preflight не должна ждать tx.
			d.fake.mu.Lock()
			d.fake.beforeNamespaceGet = mutate
			d.fake.mu.Unlock()
			_, err := d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
			if !changed || err == nil || p.calls.Load() != 0 {
				t.Fatalf("stale preflight admitted: changed=%v err=%v calls=%d", changed, err, p.calls.Load())
			}
			grant, err := d.srv.store.GetEphemeralChildCreateGrant(context.Background(), i.ID)
			if err != nil || grant.State != "queued" {
				t.Fatalf("pre-grant refusal changed hold: %+v %v", grant, err)
			}
		})
	}
}

func TestEphemeralCreateCanceledGateDoesNotLeak(t *testing.T) {
	d, i, a, p := ephemeralCreateFixture(t)
	release, err := d.srv.managed.effects.acquire(context.Background(), a.UID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = d.srv.createAllocatedEphemeralChild(ctx, d.master.DelegatorID, i.Parent, i.ID)
	release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled gate: %v", err)
	}
	_, err = d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
	if err != nil || p.creates != 1 {
		t.Fatalf("gate leaked: %v creates=%d", err, p.creates)
	}
}
func restoreLegacy51ChildHolds(t *testing.T, target *sql.DB) {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join("..", "store", "testdata", "schema-51.sql"))
	if err != nil {
		t.Fatal(err)
	}
	reference, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "reference51.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	if _, err := reference.Exec(string(fixture)); err != nil {
		t.Fatal(err)
	}
	// Только исторический DDL: данные действующего parent/job остаются исходными.
	for _, name := range []string{"swarm_steps_child_close", "swarm_steps_child_insert", "swarms_child_close"} {
		var statement string
		if err := reference.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&statement); err != nil {
			t.Fatal(err)
		}
		if _, err := target.Exec("DROP TRIGGER " + name); err != nil {
			t.Fatal(err)
		}
		if _, err := target.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEphemeralCreateMigrated51Legacy(t *testing.T) {
	d, i, _, p := ephemeralCreateFixture(t)
	path := filepath.Join(t.TempDir(), "legacy.db")
	// Получаем именно схему 51 с нашим полностью действующим parent/allocation,
	// а не fixture, которая отказала бы раньше из-за чужого пользователя или job.
	createSQL(t, d, `VACUUM INTO ?`, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`DROP TABLE project_ownership; DROP TABLE pipeline_policy_review_evidence; DROP TABLE pipeline_policy_review_assignments; DROP TABLE pipeline_policy_review_requirements; DROP TABLE swarm_eval_replays; DROP TABLE swarm_eval_bindings; DROP TRIGGER ephemeral_executor_failed_task_guard; DROP TABLE ephemeral_child_executor_failures; DROP TABLE owner_managed_actor_lineage; DROP TRIGGER ephemeral_failed_child_insert_guard; DROP TRIGGER ephemeral_failed_allocation_guard; DROP TRIGGER ephemeral_failed_attestation_guard; DROP TRIGGER ephemeral_failed_attestation_proof_guard; DROP TABLE ephemeral_child_failures; DROP TABLE ephemeral_child_lifecycles; DROP TABLE ephemeral_child_result_seals; DROP TRIGGER ephemeral_child_create_grant_initialize; DROP TABLE ephemeral_child_create_grants`)
	if err == nil {
		restoreLegacy51ChildHolds(t, db)
		_, err = db.Exec(`PRAGMA user_version=51`)
	}
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	d.srv.actorDelivery.close()
	old := d.srv.store
	d.srv.store = legacy
	defer func() { d.srv.store = old }()
	g, err := legacy.GetEphemeralChildCreateGrant(context.Background(), i.ID)
	if err != nil || g.State != "legacy" {
		t.Fatalf("legacy: %+v %v", g, err)
	}
	_, err = d.srv.createAllocatedEphemeralChild(context.Background(), d.master.DelegatorID, i.Parent, i.ID)
	if err == nil || p.calls.Load() != 0 {
		t.Fatalf("legacy effect: %v %d", err, p.creates)
	}
}
