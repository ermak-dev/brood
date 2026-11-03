package server

// FEATURE: Catalog start and reset lifecycle commands (#149).
// SPEC: start resumes a stopped managed actor in its retained container and is
// idempotent for a running one; reset replaces the container and workspace with
// a new generation and requires confirm=true. An operator must manage the swarm;
// a master acts only on its own delegated workers, rechecked after provider
// inspection. Coordination and workspace reservations refuse both before any
// provider effect.
// IMPLEMENTATION: The shared lifecycle executor admits the same durable managed
// operations as the panel's resume/reset, behind stop/delete authorization.
// RESULT: IMPLEMENTED — operator HTTP and guest RPC start/reset, ownership,
// revocation, confirmation and coordination refusals pass on the managed fake.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type providerEffects struct{ creates, starts, stops, deletes, bootstraps int }

func providerEffectsOf(pve *managedFakePVE) providerEffects {
	pve.mu.Lock()
	defer pve.mu.Unlock()
	return providerEffects{pve.creates, pve.starts, pve.stops, pve.deletes, pve.bootstraps}
}

// lifecycleCaller runs one catalog command and reports acceptance, the reason
// text and the structured result.
type lifecycleCaller func(t *testing.T, tool, args string) (bool, string, map[string]any)

func operatorLifecycle(ts *substrateServer, client *http.Client, swarm string) lifecycleCaller {
	return func(t *testing.T, tool, args string) (bool, string, map[string]any) {
		t.Helper()
		var arguments map[string]any
		if err := json.Unmarshal([]byte(args), &arguments); err != nil {
			t.Fatal(err)
		}
		r, b, raw := ts.do(client, "POST", "/api/commands/"+tool, ts.csrf(client), map[string]any{"swarm_id": swarm, "arguments": arguments})
		if r.StatusCode < 400 {
			data, _ := b["data"].(map[string]any)
			if r.StatusCode != http.StatusAccepted || b["outcome"] != "accepted" || data == nil {
				t.Fatalf("%s overstated or lost its result: %d %s", tool, r.StatusCode, raw)
			}
			return true, raw, data
		}
		return false, raw, nil
	}
}

func guestLifecycle(ts *substrateServer, from store.ManagedActor) lifecycleCaller {
	return func(t *testing.T, tool, args string) (bool, string, map[string]any) {
		t.Helper()
		got := ts.srv.managed.handleSwarmRequest(context.Background(), from.UID, from.Incarnation, relay.SwarmRequest{Tool: tool, Arguments: json.RawMessage(args)})
		if !got.OK {
			return false, got.Text, nil
		}
		var data map[string]any
		if got.Outcome != "accepted" || json.Unmarshal(got.Data, &data) != nil {
			t.Fatalf("%s overstated or lost its result: %+v", tool, got)
		}
		return true, got.Text, data
	}
}

// settleActor reconciles only this actor: the provider fake holds one container.
func settleActor(t *testing.T, ts *substrateServer, uid string, done func(store.ManagedActor) bool) store.ManagedActor {
	t.Helper()
	ctx := context.Background()
	for range 12 {
		a, err := ts.srv.store.GetManagedActor(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if done(a) {
			return a
		}
		if _, err := ts.srv.managed.reconcileActor(ctx, a); err != nil {
			t.Fatalf("reconcile %s: %v (%+v)", a.Name, err, a)
		}
	}
	a, _ := ts.srv.store.GetManagedActor(ctx, uid)
	t.Fatalf("actor did not settle: %+v", a)
	return a
}

func actorRunning(a store.ManagedActor) bool {
	return a.Phase == "running" && a.Desired == "running" && a.Operation == "start" && a.Stage == "done"
}

func actorStopped(a store.ManagedActor) bool {
	return a.Phase == "stopped" && a.Desired == "stopped" && a.Stage == "done"
}

func stoppedLifecycleWorker(t *testing.T, ts *substrateServer, master store.ManagedActor, pve *managedFakePVE) store.ManagedActor {
	t.Helper()
	worker := lifecycleWorker(t, ts, master, pve)
	if _, err := ts.srv.executeActorCommand(context.Background(), master.DelegatorID, master.SwarmID, lifecycleInvocation("stop", `{"name":"worker"}`)); err != nil {
		t.Fatal(err)
	}
	return settleActor(t, ts, worker.UID, actorStopped)
}

// expectUnchanged proves a refusal wrote neither intent nor provider state.
func expectUnchanged(t *testing.T, ts *substrateServer, pve *managedFakePVE, before store.ManagedActor, effects providerEffects) {
	t.Helper()
	after, err := ts.srv.store.GetManagedActor(context.Background(), before.UID)
	if err != nil || after.Revision != before.Revision || after.OperationID != before.OperationID || after.Incarnation != before.Incarnation {
		t.Fatalf("refusal changed actor: %+v %v", after, err)
	}
	if got := providerEffectsOf(pve); got != effects {
		t.Fatalf("refusal reached provider: %+v -> %+v", effects, got)
	}
}

func lifecycleCallers(ts *substrateServer, alice *http.Client, master store.ManagedActor) map[string]lifecycleCaller {
	return map[string]lifecycleCaller{"operator": operatorLifecycle(ts, alice, master.SwarmID), "master": guestLifecycle(ts, master)}
}

func TestStartCommandResumesStoppedActor(t *testing.T) {
	for _, via := range []string{"operator", "master"} {
		t.Run(via, func(t *testing.T) {
			ts, alice, master, pve := masterSetup(t)
			worker := stoppedLifecycleWorker(t, ts, master, pve)
			call := lifecycleCallers(ts, alice, master)[via]
			before := providerEffectsOf(pve)
			ok, text, data := call(t, "start", `{"name":"worker"}`)
			if !ok || data["uid"] != worker.UID || data["operation"] != "start" || data["desired"] != "running" || !strings.Contains(text, "asynchronous") {
				t.Fatalf("start: %v %s %v", ok, text, data)
			}
			after, err := ts.srv.store.GetManagedActor(context.Background(), worker.UID)
			if err != nil || after.Operation != "start" || after.Desired != "running" || after.OperationID == worker.OperationID || after.CTID != worker.CTID || after.OwnerNonce != worker.OwnerNonce || after.Incarnation != worker.Incarnation || after.Resources != worker.Resources {
				t.Fatalf("start lost retained allocation: %+v %v", after, err)
			}
			want := before
			want.starts++
			want.bootstraps++
			if got := providerEffectsOf(pve); got != want {
				t.Fatalf("start provider effects: %+v want %+v", got, want)
			}
			running := settleActor(t, ts, worker.UID, actorRunning)
			if running.Incarnation != worker.Incarnation || running.CTID != worker.CTID {
				t.Fatalf("start replaced the container: %+v", running)
			}
			// Повторный start работающего актора — no-op без нового намерения.
			effects := providerEffectsOf(pve)
			ok, text, data = call(t, "start", `{"name":"worker"}`)
			if !ok || data["state"] != "running" || data["operation_id"] != running.OperationID {
				t.Fatalf("idempotent start: %v %s %v", ok, text, data)
			}
			expectUnchanged(t, ts, pve, running, effects)
		})
	}
}

func TestResetCommandReplacesGeneration(t *testing.T) {
	for _, via := range []string{"operator", "master"} {
		t.Run(via, func(t *testing.T) {
			ts, alice, master, pve := masterSetup(t)
			worker := lifecycleWorker(t, ts, master, pve)
			call := lifecycleCallers(ts, alice, master)[via]
			before := providerEffectsOf(pve)
			for _, args := range []string{`{"name":"worker"}`, `{"name":"worker","confirm":false}`} {
				if ok, text, _ := call(t, "reset", args); ok || !strings.Contains(text, "confirm=true") {
					t.Fatalf("unconfirmed reset: %v %s", ok, text)
				}
			}
			expectUnchanged(t, ts, pve, worker, before)
			ok, text, data := call(t, "reset", `{"name":"worker","confirm":true}`)
			if !ok || data["uid"] != worker.UID || data["operation"] != "reset" || data["desired"] != "running" {
				t.Fatalf("reset: %v %s %v", ok, text, data)
			}
			after, err := ts.srv.store.GetManagedActor(context.Background(), worker.UID)
			if err != nil || after.Operation != "reset" || after.Desired != "running" || after.OperationID == worker.OperationID || after.Incarnation != worker.Incarnation {
				t.Fatalf("reset intent: %+v %v", after, err)
			}
			replaced := settleActor(t, ts, worker.UID, func(a store.ManagedActor) bool {
				return actorRunning(a) && a.Incarnation == worker.Incarnation+1
			})
			if replaced.UID != worker.UID || replaced.CTID != worker.CTID || replaced.OwnerNonce == worker.OwnerNonce || replaced.Resources != worker.Resources || replaced.ParentUID != master.UID {
				t.Fatalf("reset did not replace the exact allocation: %+v", replaced)
			}
			want := before
			want.creates++
			want.stops++
			want.deletes++
			want.starts++
			want.bootstraps++
			if got := providerEffectsOf(pve); got != want {
				t.Fatalf("reset provider effects: %+v want %+v", got, want)
			}
		})
	}
}

// threeSlotMasters builds two approved masters and a worker created by the
// second. The default fixture pool holds only two actors.
func threeSlotMasters(t *testing.T) (ts *substrateServer, alice *http.Client, first, second, theirs store.ManagedActor, pve *managedFakePVE) {
	t.Helper()
	env := runtimeFixture(t)
	var pools map[string]map[string]any
	if err := json.Unmarshal([]byte(env[proxmoxPoolsSetting]), &pools); err != nil {
		t.Fatal(err)
	}
	pools["brood-proxmox"]["capacity"] = map[string]any{"actors": 3, "cpuMillicores": "3000", "memoryBytes": "3221225472", "diskGiB": 12}
	env[proxmoxPoolsSetting] = proxmoxPoolsJSON(t, pools)
	cfg, err := loadProxmox(env)
	if err != nil {
		t.Fatal(err)
	}
	pve = &managedFakePVE{}
	hub := &managedFakeRelay{connected: true}
	ts = newSubstrateServer(t, newFakeAteapi(), func(c *Config) { c.proxmox = cfg.proxmox })
	ts.srv.managed.stopReconcile()
	ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return pve, nil }
	if ts.srv.managed.hub != nil {
		ts.srv.managed.hub.Close()
	}
	ts.srv.managed.hub = hub
	hub.authorize = ts.srv.managed.authorizeGuest
	pve.server = ts.srv
	admin, alice, _, swarm := actorsSetup(t, ts.testServer)
	r, b, _ := ts.do(admin, "POST", "/api/proxmox-pools/brood-proxmox/activate", ts.csrf(admin), nil)
	expectStatus(t, "activate", r, b, 201)
	ctx := context.Background()
	u, err := ts.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetUserAdmin(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	master := func(name string, id int) store.ManagedActor {
		pve.nextIDs, pve.running = []int{id}, false
		r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": name}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}, "role": "master"})
		expectStatus(t, name, r, b, 201)
		a, err := ts.srv.store.LookupManagedActor(ctx, swarm, name)
		if err != nil {
			t.Fatal(err)
		}
		return settleActor(t, ts, a.UID, actorRunning)
	}
	first, second = master("master", 105), master("other", 106)
	pve.nextIDs, pve.running = []int{107}, false
	if _, err := ts.srv.managed.executeLifecycleCommand(ctx, second, lifecycleInvocation("add", `{"name":"theirs"}`)); err != nil {
		t.Fatal(err)
	}
	theirs, err = ts.srv.store.LookupManagedActor(ctx, swarm, "theirs")
	if err != nil || theirs.ParentUID != second.UID {
		t.Fatalf("theirs: %+v %v", theirs, err)
	}
	theirs = settleActor(t, ts, theirs.UID, actorRunning)
	return ts, alice, first, second, theirs, pve
}

func TestStartResetRefuseAnotherMastersWorker(t *testing.T) {
	ts, _, master, other, theirs, pve := threeSlotMasters(t)
	ctx := context.Background()
	if _, err := ts.srv.executeActorCommand(ctx, master.DelegatorID, master.SwarmID, lifecycleInvocation("stop", `{"name":"theirs"}`)); err != nil {
		t.Fatal(err)
	}
	theirs = settleActor(t, ts, theirs.UID, actorStopped)
	effects := providerEffectsOf(pve)
	intruder := guestLifecycle(ts, master)
	for _, call := range [][2]string{{"start", `{"name":"theirs"}`}, {"reset", `{"name":"theirs","confirm":true}`}} {
		if ok, text, _ := intruder(t, call[0], call[1]); ok || !strings.Contains(text, "not authorized") {
			t.Fatalf("%s of another master's worker: %v %s", call[0], ok, text)
		}
	}
	expectUnchanged(t, ts, pve, theirs, effects)
	// Отказ — из-за владения, а не состояния: свой master тот же start выполняет.
	if ok, text, _ := guestLifecycle(ts, other)(t, "start", `{"name":"theirs"}`); !ok {
		t.Fatalf("owning master start: %s", text)
	}
}

var startResetArguments = map[string]string{"start": `{"name":"worker"}`, "reset": `{"name":"worker","confirm":true}`}

func TestStartResetRefuseWorkerCaller(t *testing.T) {
	ts, _, master, pve := masterSetup(t)
	worker := lifecycleWorker(t, ts, master, pve)
	master, err := ts.srv.store.GetManagedActor(context.Background(), master.UID)
	if err != nil {
		t.Fatal(err)
	}
	effects := providerEffectsOf(pve)
	// Worker без делегации не управляет ни собой, ни master.
	for _, target := range []string{"worker", "master"} {
		for _, tool := range []string{"start", "reset"} {
			args := strings.ReplaceAll(startResetArguments[tool], "worker", target)
			if ok, text, _ := guestLifecycle(ts, worker)(t, tool, args); ok {
				t.Fatalf("worker %s of %s accepted: %s", tool, target, text)
			}
		}
	}
	expectUnchanged(t, ts, pve, worker, effects)
	expectUnchanged(t, ts, pve, master, effects)
	if ok, text, _ := guestLifecycle(ts, master)(t, "reset", startResetArguments["reset"]); !ok {
		t.Fatalf("master reset of its worker: %s", text)
	}
}

func TestStartResetRefuseUnauthorizedCallers(t *testing.T) {
	for _, scenario := range []string{"revoked", "disabled", "non-manager"} {
		t.Run(scenario, func(t *testing.T) {
			ts, alice, master, pve := masterSetup(t)
			worker := stoppedLifecycleWorker(t, ts, master, pve)
			ctx := context.Background()
			call := guestLifecycle(ts, master)
			restore := func() {}
			switch scenario {
			case "revoked":
				if err := ts.srv.store.SetUserAdmin(ctx, master.DelegatorID, false); err != nil {
					t.Fatal(err)
				}
				restore = func() {
					if err := ts.srv.store.SetUserAdmin(ctx, master.DelegatorID, true); err != nil {
						t.Fatal(err)
					}
				}
			case "disabled":
				if err := ts.srv.store.SetUserDisabled(ctx, master.DelegatorID, true); err != nil {
					t.Fatal(err)
				}
				restore = func() {
					if err := ts.srv.store.SetUserDisabled(ctx, master.DelegatorID, false); err != nil {
						t.Fatal(err)
					}
				}
			case "non-manager":
				bob := newClient(t)
				ts.login(bob, "bob", "bob-password-1")
				call = operatorLifecycle(ts, bob, master.SwarmID)
				restore = func() { call = operatorLifecycle(ts, alice, master.SwarmID) }
			}
			effects := providerEffectsOf(pve)
			for _, tool := range []string{"start", "reset"} {
				if ok, text, _ := call(t, tool, startResetArguments[tool]); ok {
					t.Fatalf("%s accepted for %s: %s", tool, scenario, text)
				}
			}
			expectUnchanged(t, ts, pve, worker, effects)
			restore()
			if ok, text, _ := call(t, "start", startResetArguments["start"]); !ok {
				t.Fatalf("restored caller start: %s", text)
			}
		})
	}
}

func TestStartResetRecheckApprovalAfterInspection(t *testing.T) {
	for _, tool := range []string{"start", "reset"} {
		t.Run(tool, func(t *testing.T) {
			ts, _, master, pve := masterSetup(t)
			worker := stoppedLifecycleWorker(t, ts, master, pve)
			ctx := context.Background()
			ts.srv.managed.clients = map[string]proxmoxAPI{}
			ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
				return lifecycleInspect{proxmoxAPI: pve, inspect: func(ctx context.Context, ref proxmox.OwnedCT) (proxmox.CTConfig, error) {
					config, err := pve.InspectOwned(ctx, ref)
					if err == nil {
						err = ts.srv.store.SetUserAdmin(ctx, master.DelegatorID, false)
					}
					return config, err
				}}, nil
			}
			effects := providerEffectsOf(pve)
			if ok, text, _ := guestLifecycle(ts, master)(t, tool, startResetArguments[tool]); ok {
				t.Fatalf("%s accepted after revocation during inspection: %s", tool, text)
			}
			expectUnchanged(t, ts, pve, worker, effects)
			// Без отзыва тот же путь проходит: отказ дала именно повторная проверка.
			ts.srv.managed.clients = map[string]proxmoxAPI{}
			ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return pve, nil }
			if err := ts.srv.store.SetUserAdmin(ctx, master.DelegatorID, true); err != nil {
				t.Fatal(err)
			}
			if ok, text, _ := guestLifecycle(ts, master)(t, tool, startResetArguments[tool]); !ok {
				t.Fatalf("%s without revocation: %s", tool, text)
			}
		})
	}
}

func TestStartResetRefuseHeldCoordination(t *testing.T) {
	for _, hold := range []string{"dispatch", "workspace"} {
		for _, via := range []string{"operator", "master"} {
			t.Run(hold+"/"+via, func(t *testing.T) {
				ts, alice, master, pve := masterSetup(t)
				worker := lifecycleWorker(t, ts, master, pve)
				ctx := context.Background()
				sw, err := ts.srv.store.GetSwarm(ctx, master.SwarmID)
				if err != nil {
					t.Fatal(err)
				}
				release := func() {}
				switch hold {
				case "dispatch":
					ts.srv.managed.deliverMessage = func(context.Context, string, int64, relay.MessageRequest) (relay.MessageResponse, error) {
						return relay.MessageResponse{OK: true}, nil
					}
					if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, coordinationInvocation("dispatch", map[string]any{"to": "worker", "message": "task", "request_id": "held"})); err != nil {
						t.Fatal(err)
					}
					release = func() {
						claim, err := ts.srv.store.ShowDispatchLease(ctx, sw.ProjectID, sw.ID, worker.UID)
						if err != nil || claim.RequestID != "held" {
							t.Fatalf("refusal lost the lease: %+v %v", claim, err)
						}
						if err := ts.srv.store.ReleaseDispatchLease(ctx, claim); err != nil {
							t.Fatal(err)
						}
					}
				case "workspace":
					op, err := ts.srv.store.AcquireActorOperation(ctx, worker.UID, worker.Incarnation, store.NewID(), "operator:test", "restart")
					if err != nil {
						t.Fatal(err)
					}
					release = func() {
						if err := ts.srv.store.FinishActorOperation(ctx, op, true); err != nil {
							t.Fatal(err)
						}
					}
				}
				call := lifecycleCallers(ts, alice, master)[via]
				effects := providerEffectsOf(pve)
				if ok, text, _ := call(t, "reset", `{"name":"worker","confirm":true}`); ok {
					t.Fatalf("reset with held %s: %s", hold, text)
				}
				// Неготовый гость (desired running, phase error) — тот случай, где
				// start восстанавливает работающий CT; удержание всё равно в силе.
				worker, err = ts.srv.store.GetManagedActor(ctx, worker.UID)
				if err != nil {
					t.Fatal(err)
				}
				if worker, err = ts.srv.managed.saveObservation(ctx, worker, "error", "", managedReadinessError); err != nil {
					t.Fatal(err)
				}
				if ok, text, _ := call(t, "start", `{"name":"worker"}`); ok {
					t.Fatalf("start with held %s: %s", hold, text)
				}
				expectUnchanged(t, ts, pve, worker, effects)
				release()
				if ok, text, _ := call(t, "reset", `{"name":"worker","confirm":true}`); !ok {
					t.Fatalf("reset after release: %s", text)
				}
			})
		}
	}
}
