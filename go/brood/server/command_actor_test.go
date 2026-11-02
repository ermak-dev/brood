package server

// FEATURE: Managed lifecycle commands share durable workload services.
// SPEC: Revalidate UID/generation and delegated approval, confine targets to the
// swarm, retain stopped allocations, require destructive confirmation, and never
// disguise reset as restart/update/recycle/wipe.
// IMPLEMENTATION: Shared creation/admission services, strict catalog arguments,
// current target identity, and approval revalidation after provider inspection.
// RESULT: IMPLEMENTED — durable intents, quota retention, confirmation, and scope
// fences verified; unsupported semantics fail without modifying allocations.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

func lifecycleInvocation(name, arguments string) command.Invocation {
	return command.Invocation{Name: name, Arguments: json.RawMessage(arguments)}
}

func TestActorCommandStopDurableAndDeleteConfirmed(t *testing.T) {
	ts, _, master, pve := masterSetup(t)
	ctx := context.Background()
	result, err := ts.srv.managed.executeLifecycleCommand(ctx, master, lifecycleInvocation("stop", `{"name":"master"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Outcome) != "accepted" || !strings.Contains(result.Text, "asynchronous") {
		t.Fatalf("completion overstated: %+v", result)
	}
	var data map[string]any
	if err := json.Unmarshal(result.Data, &data); err != nil || data["uid"] != master.UID {
		t.Fatalf("result identity: %s %v", result.Data, err)
	}
	stopped, err := ts.srv.store.GetManagedActor(ctx, master.UID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Operation != "stop" || stopped.Desired != "stopped" || stopped.OperationID == master.OperationID || stopped.Stage != "stop" || stopped.UPID == "" || stopped.CTID != master.CTID || stopped.OwnerNonce != master.OwnerNonce || stopped.Incarnation != master.Incarnation || stopped.Resources != master.Resources || pve.stops != 1 || pve.deletes != 0 {
		t.Fatalf("stop lost durable allocation: %+v", stopped)
	}
	for range 2 {
		ts.srv.managed.reconcileOnce(ctx)
	}
	stopped, _ = ts.srv.store.GetManagedActor(ctx, master.UID)
	if stopped.Phase != "stopped" {
		t.Fatalf("stop not reconciled: %+v", stopped)
	}
	sw, err := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	reservations, err := ts.srv.shardReservations(ctx, []app.Swarm{toAppSwarm(sw)})
	if err != nil || len(reservations) != 1 || reservations[0].CPUMillicores != master.Resources.CPUMillicores || reservations[0].MemoryBytes != master.Resources.MemoryBytes {
		t.Fatalf("stopped quota lost: %+v %v", reservations, err)
	}
	// Оператор может удалить остановленного master без гостевого полномочия.
	before := stopped
	if _, err := ts.srv.executeActorCommand(ctx, master.DelegatorID, master.SwarmID, lifecycleInvocation("delete", `{"name":"master"}`)); err == nil {
		t.Fatal("unconfirmed disk deletion accepted")
	}
	after, _ := ts.srv.store.GetManagedActor(ctx, master.UID)
	if after.Revision != before.Revision {
		t.Fatal("unconfirmed deletion mutated intent")
	}
	result, err = ts.srv.executeActorCommand(ctx, master.DelegatorID, master.SwarmID, lifecycleInvocation("delete", `{"name":"master","confirm":true}`))
	if err != nil || string(result.Outcome) != "accepted" {
		t.Fatalf("delete: %+v %v", result, err)
	}
	after, _ = ts.srv.store.GetManagedActor(ctx, master.UID)
	if after.Desired != "deleted" || after.Operation != "delete" || after.OperationID == before.OperationID {
		t.Fatalf("delete not durable: %+v", after)
	}
}

func TestActorCommandAddInheritsAndRetries(t *testing.T) {
	ts, _, master, pve := masterSetup(t)
	pve.nextIDs = []int{106}
	pve.uncertain = true
	ctx := context.Background()
	inv := lifecycleInvocation("add", `{"name":"child"}`)
	result, err := ts.srv.managed.executeLifecycleCommand(ctx, master, inv)
	if err != nil || string(result.Outcome) != "accepted" {
		t.Fatalf("add: %+v %v", result, err)
	}
	child, err := ts.srv.store.LookupManagedActor(ctx, master.SwarmID, "child")
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentUID != master.UID || child.PoolUID != master.PoolUID || child.Role != "worker" || child.DelegatorID != "" || child.Template != master.Template || child.SourceFingerprint != master.SourceFingerprint || child.OperationID == "" {
		t.Fatalf("approval not inherited: %+v", child)
	}
	creates := pve.creates
	if _, err := ts.srv.managed.executeLifecycleCommand(ctx, master, inv); err != nil {
		t.Fatal(err)
	}
	if pve.creates != creates {
		t.Fatal("retry duplicated reservation")
	}
}

func TestActorCommandRejectsUnsupportedAndInvalidArguments(t *testing.T) {
	ts, _, master, pve := masterSetup(t)
	ctx := context.Background()
	for _, name := range []string{"restart", "update", "recycle", "wipe"} {
		args := `{"name":"master"}`
		if name == "recycle" || name == "wipe" {
			args = `{"name":"master","confirm":true}`
		}
		_, err := ts.srv.managed.executeLifecycleCommand(ctx, master, lifecycleInvocation(name, args))
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("%s misrepresented: %v", name, err)
		}
	}
	for _, args := range []string{`{}`, `null`, `[]`, `{"name":""}`, `{"name":"../master"}`, `{"name":"master","pool":"other"}`, `{"name":"master","role":"master"}`, `{"name":"master","confirm":"yes"}`, `{"name":"master","name":"other"}`, `{"name":"master"} {}`} {
		if _, err := ts.srv.managed.executeLifecycleCommand(ctx, master, lifecycleInvocation("stop", args)); err == nil {
			t.Errorf("invalid args accepted: %s", args)
		}
	}
	a, _ := ts.srv.store.GetManagedActor(ctx, master.UID)
	if a.Revision != master.Revision || pve.stops != 0 || pve.deletes != 0 {
		t.Fatal("rejected command changed actor")
	}
}

func TestActorCommandAuthorizationBoundaries(t *testing.T) {
	for _, scenario := range []string{"stale-generation", "wrong-swarm", "revoked", "disabled", "namespace", "worker-peer", "cross-swarm-target"} {
		t.Run(scenario, func(t *testing.T) {
			ts, _, master, pve := masterSetup(t)
			ctx := context.Background()
			original := master
			target := "master"
			var err error
			switch scenario {
			case "stale-generation":
				original.Incarnation++
			case "wrong-swarm":
				original.SwarmID = "foreign"
			case "revoked":
				err = ts.srv.store.SetUserAdmin(ctx, master.DelegatorID, false)
			case "disabled":
				err = ts.srv.store.SetUserDisabled(ctx, master.DelegatorID, true)
			case "namespace":
				sw, _ := ts.srv.store.GetSwarm(ctx, master.SwarmID)
				ts.fake.mu.Lock()
				ts.fake.atespaces[sw.AtespaceName].Metadata.Uid = "replacement"
				ts.fake.mu.Unlock()
			case "worker-peer":
				original = lifecycleWorker(t, ts, master, pve)
			case "cross-swarm-target":
				target = "foreign"
				sw, e := ts.srv.store.GetSwarm(ctx, master.SwarmID)
				if e != nil {
					t.Fatal(e)
				}
				sw.ID, sw.Name, sw.AtespaceName = store.NewID(), "foreign", "foreign-space"
				foreignUID := "foreign-namespace-uid"
				sw.AtespaceUID = &foreignUID
				if e = ts.srv.store.CreateSwarm(ctx, sw); e != nil {
					t.Fatal(e)
				}
				_, err = ts.srv.store.ReserveManagedActor(ctx, store.ManagedActor{UID: store.NewID(), Name: target, SwarmID: sw.ID, AtespaceUID: foreignUID, PoolUID: master.PoolUID, CTID: 106, Resources: master.Resources, OwnerNonce: store.NewID(), OperationID: store.NewID()})
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = ts.srv.managed.executeLifecycleCommand(ctx, original, lifecycleInvocation("stop", `{"name":"`+target+`"}`))
			if err == nil {
				t.Fatal("unauthorized mutation accepted")
			}
			after, _ := ts.srv.store.GetManagedActor(ctx, master.UID)
			if after.Revision != master.Revision || pve.stops != 0 {
				t.Fatalf("refusal changed target: %+v", after)
			}
		})
	}
}

type lifecycleInspect struct {
	proxmoxAPI
	inspect func(context.Context, proxmox.OwnedCT) (proxmox.CTConfig, error)
}

func (p lifecycleInspect) InspectOwned(ctx context.Context, ref proxmox.OwnedCT) (proxmox.CTConfig, error) {
	return p.inspect(ctx, ref)
}

func TestActorCommandRechecksApprovalAfterInspection(t *testing.T) {
	ts, _, master, pve := masterSetup(t)
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
	_, err := ts.srv.managed.executeLifecycleCommand(context.Background(), master, lifecycleInvocation("stop", `{"name":"master"}`))
	if err == nil {
		t.Fatal("revoked during inspection accepted")
	}
	after, _ := ts.srv.store.GetManagedActor(context.Background(), master.UID)
	if after.Revision != master.Revision || pve.stops != 0 {
		t.Fatalf("revocation did not fence durable intent: %+v", after)
	}
}

func lifecycleWorker(t *testing.T, ts *substrateServer, master store.ManagedActor, pve *managedFakePVE) store.ManagedActor {
	t.Helper()
	ctx := context.Background()
	pve.nextIDs = []int{106}
	pve.running = false
	if _, err := ts.srv.managed.executeLifecycleCommand(ctx, master, lifecycleInvocation("add", `{"name":"worker"}`)); err != nil {
		t.Fatal(err)
	}
	worker, err := ts.srv.store.LookupManagedActor(ctx, master.SwarmID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		worker, err = ts.srv.managed.reconcileActor(ctx, worker)
		if err != nil {
			t.Fatal(err)
		}
	}
	if worker.Phase != "running" {
		t.Fatalf("worker not running: %+v", worker)
	}
	return worker
}

func TestActorCommandMasterMutatesPeer(t *testing.T) {
	ts, _, master, pve := masterSetup(t)
	worker := lifecycleWorker(t, ts, master, pve)
	ctx := context.Background()
	result, err := ts.srv.managed.executeLifecycleCommand(ctx, master, lifecycleInvocation("stop", `{"name":"worker"}`))
	if err != nil || result.Outcome != "accepted" {
		t.Fatalf("peer stop: %+v %v", result, err)
	}
	after, err := ts.srv.store.GetManagedActor(ctx, worker.UID)
	if err != nil || after.Operation != "stop" || after.Desired != "stopped" || after.OperationID == worker.OperationID || after.CTID != worker.CTID {
		t.Fatalf("peer intent: %+v %v", after, err)
	}
	for range 2 {
		after, err = ts.srv.managed.reconcileActor(ctx, after)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ts.srv.managed.executeLifecycleCommand(ctx, master, lifecycleInvocation("delete", `{"name":"worker","confirm":true}`)); err != nil {
		t.Fatal(err)
	}
	after, err = ts.srv.store.GetManagedActor(ctx, worker.UID)
	if err != nil || after.Operation != "delete" || after.Desired != "deleted" {
		t.Fatalf("peer delete intent: %+v %v", after, err)
	}
	if pve.deletes != 0 {
		t.Fatal("delete skipped durable stop stage")
	}
}

func TestActorCommandWorkerTargetScope(t *testing.T) {
	ts, _, master, pve := masterSetup(t)
	worker := lifecycleWorker(t, ts, master, pve)
	ctx := context.Background()
	if _, err := ts.srv.managed.authorizeCommandTarget(ctx, worker, "worker", false); err != nil {
		t.Fatalf("worker self read: %v", err)
	}
	if _, err := ts.srv.managed.authorizeCommandTarget(ctx, worker, "master", false); !errors.Is(err, app.ErrNotAllowed) {
		t.Fatalf("worker peer session access: %v", err)
	}
	if _, err := ts.srv.managed.executeLifecycleCommand(ctx, worker, lifecycleInvocation("stop", `{"name":"worker"}`)); err == nil {
		t.Fatal("worker lifecycle policy not explicit")
	}
}

func TestActorCommandTargetHelperIdentity(t *testing.T) {
	ts, _, master, _ := masterSetup(t)
	ctx := context.Background()
	target, err := ts.srv.managed.authorizeCommandTarget(ctx, master, "master", false)
	if err != nil || target.UID != master.UID {
		t.Fatalf("target: %+v %v", target, err)
	}
	master.Incarnation++
	if _, err = ts.srv.managed.authorizeCommandTarget(ctx, master, "master", false); !errors.Is(err, app.ErrNotAllowed) {
		t.Fatalf("stale target read: %v", err)
	}
}

func TestActorCommandOperatorACL(t *testing.T) {
	ts, _, master, _ := masterSetup(t)
	ctx := context.Background()
	bob, err := ts.srv.store.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ts.srv.executeActorCommand(ctx, bob.ID, master.SwarmID, lifecycleInvocation("stop", `{"name":"master"}`))
	if !errors.Is(err, app.ErrNotAllowed) {
		t.Fatalf("operator authorization: %v", err)
	}
	if _, err = ts.srv.executeActorCommand(ctx, master.DelegatorID, master.SwarmID, lifecycleInvocation("add", `{"name":"child"}`)); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("operator add guessed profile: %v", err)
	}
	a, _ := ts.srv.store.GetManagedActor(ctx, master.UID)
	if a.Revision != master.Revision {
		t.Fatal("operator refusal changed actor")
	}
}
