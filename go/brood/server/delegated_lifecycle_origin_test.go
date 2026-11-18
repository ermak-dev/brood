package server

// BUG: Явная lifecycle-команда master блокируется автоматической workspace-арендой.
// HYPOTHESIS: parent-ветка beginLifecycle не сохраняет положительный manual origin.
// SOLUTION: Текущая делегация и binding проверяются при записи actor intervention.
// RESULT: FIXED — guest relay принимает явный intent до эффекта, holds сохранены.
// STATUS: FIXED

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

type delegatedJournalPVE struct {
	proxmoxAPI
	beforeStop func()
}

func (p *delegatedJournalPVE) Stop(ctx context.Context, owned proxmox.OwnedCT) (string, error) {
	p.beforeStop()
	return p.proxmoxAPI.Stop(ctx, owned)
}

func TestDelegatedLifecycleGuestInterruptsRunningChildBeforeEffects(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			ctx := context.Background()
			d, actor, child := runningEphemeralChildReportFixture(t)
			d.worker = actor
			parent, _, _ := childRelay(t, d)
			pool, err := d.srv.store.GetManagedPool(ctx, actor.PoolUID)
			if err != nil {
				t.Fatal(err)
			}
			runtime := d.srv.managed.registry.pools[pool.Connection].runtime
			runtime.actors = pool.MaxActors
			runtime.cpuMillicores = pool.Resources.CPUMillicores
			runtime.memoryBytes = pool.Resources.MemoryBytes
			runtime.diskGiB = pool.Resources.DiskBytes / gib
			pve := &managedFakePVE{server: d.srv, exists: true, running: true, spec: d.srv.managed.createSpec(actor), uncertainStop: uncertain}
			checked := 0
			provider := &delegatedJournalPVE{proxmoxAPI: pve, beforeStop: func() {
				checked++
				current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
				if err != nil {
					t.Fatal(err)
				}
				origin, err := d.srv.store.GetOwnerManagedActorOperation(ctx, actor.UID, actor.Incarnation, current.OperationID)
				if err != nil || origin.Caller != fmt.Sprintf("actor:%s:%d", d.master.UID, d.master.Incarnation) || origin.State != "completed" {
					t.Fatalf("effect reached before truthful accepted journal: %+v %v", origin, err)
				}
				work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
				if err != nil || work.Task != child {
					t.Fatalf("effect reached before automatic interruption: %+v %v", work, err)
				}
				join, err := d.srv.store.GetSwarmChildJoin(ctx, work.Parent, d.srv.managed.childAuthority)
				if err != nil || !join.Interrupted || join.Evaluation.State != "unconfirmed" {
					t.Fatalf("effect reached before durable join interruption: %+v %v", join, err)
				}
			}}
			d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return provider, nil }
			delete(d.srv.managed.clients, pool.UID)
			response := childCall(t, parent, "stop", map[string]any{"name": actor.Name})
			if checked != 1 || pve.stops != 1 {
				t.Fatalf("expected existing explicit effect once: checked=%d stops=%d response=%+v", checked, pve.stops, response)
			}
			// accepted подтверждает durable intent, не результат provider task.
			if !response.OK || response.Outcome != "accepted" {
				t.Fatalf("explicit child stop intent not accepted: %+v", response)
			}
			if uncertain {
				current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
				if err != nil || current.Stage != "stop" || current.UPID != "" || current.Phase != "error" || current.Diagnostic() == "" {
					t.Fatalf("unknown provider outcome lost: %+v %v", current, err)
				}
				retry := childCall(t, parent, "reset", map[string]any{"name": actor.Name, "confirm": true})
				if retry.OK || pve.stops != 1 {
					t.Fatalf("explicit control replaced unknown provider task: %+v", retry)
				}
			}
			work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
			if err != nil || work.Task != child {
				t.Fatalf("provider outcome rewrote original child: %+v %v", work, err)
			}
			if err := d.srv.store.CheckActorChildObligations(ctx, actor.UID); err == nil {
				t.Fatal("explicit stop released automatic child hold")
			}
		})
	}
}

func TestDelegatedLifecycleIntentRechecksCapturedBinding(t *testing.T) {
	for _, binding := range []string{"project", "namespace"} {
		t.Run(binding, func(t *testing.T) {
			ctx := context.Background()
			ts, _, master, pve := masterSetup(t)
			worker := lifecycleWorker(t, ts, master, pve)
			sw, err := ts.srv.store.GetSwarm(ctx, master.SwarmID)
			if err != nil {
				t.Fatal(err)
			}
			op := &managedWorkloadOperation{server: ts.srv, userID: master.DelegatorID, swarm: toAppSwarm(sw), action: "stop", parent: &master, operationID: "binding-refused"}
			db, err := sql.Open("sqlite", ts.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if binding == "project" {
				if err := ts.srv.store.CreateProject(ctx, store.Project{ID: "rebound", Name: "rebound"}); err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`UPDATE swarms SET project_id='rebound' WHERE id=?`, sw.ID)
			} else {
				_, err = db.Exec(`UPDATE swarms SET atespace_uid='rebound' WHERE id=?`, sw.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := op.beginLifecycle(ctx, worker); err == nil {
				t.Fatal("captured binding change accepted")
			}
			current, err := ts.srv.store.GetManagedActor(ctx, worker.UID)
			if err != nil || current != worker || pve.stops != 0 {
				t.Fatalf("binding refusal mutated target: %+v %v", current, err)
			}
			if _, err := ts.srv.store.GetOwnerManagedActorOperation(ctx, worker.UID, worker.Incarnation, op.operationID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("binding refusal journaled permission: %v", err)
			}
		})
	}
}

func TestDelegatedLifecycleGuestInterruptsWorkspace(t *testing.T) {
	for _, action := range []string{"start", "stop", "reset", "delete"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			ts, _, master, pve := masterSetup(t)
			worker := lifecycleWorker(t, ts, master, pve)
			automatic, err := ts.srv.store.AcquireActorOperation(ctx, worker.UID, worker.Incarnation, "workspace-inflight", "automatic", "restart")
			if err != nil {
				t.Fatal(err)
			}
			firstInterruption := ""
			if action == "start" {
				if _, err := ts.srv.managed.executeLifecycleCommand(ctx, master, lifecycleInvocation("stop", `{"name":"worker"}`)); err != nil {
					t.Fatal(err)
				}
				worker = settleActor(t, ts, worker.UID, actorStopped)
				held, err := ts.srv.store.CurrentActorOperation(ctx, worker.UID)
				if err != nil {
					t.Fatal(err)
				}
				firstInterruption = held.InterruptedBy
			}
			arguments := []byte(`{"name":"worker"}`)
			if action == "reset" || action == "delete" {
				arguments = []byte(`{"name":"worker","confirm":true}`)
			}
			response := ts.srv.managed.handleSwarmRequest(ctx, master.UID, master.Incarnation, relay.SwarmRequest{Tool: action, Arguments: arguments})
			if !response.OK {
				t.Fatalf("explicit delegated %s blocked by automatic hold: %+v", action, response)
			}
			current, err := ts.srv.store.GetManagedActor(ctx, worker.UID)
			if err != nil {
				t.Fatal(err)
			}
			origin, err := ts.srv.store.GetOwnerManagedActorOperation(ctx, worker.UID, worker.Incarnation, current.OperationID)
			if err != nil || origin.Caller != fmt.Sprintf("actor:%s:%d", master.UID, master.Incarnation) || origin.Operation != action || origin.State != "completed" {
				t.Fatalf("accepted explicit actor intent lost truthful origin: %+v %v", origin, err)
			}
			held, err := ts.srv.store.CurrentActorOperation(ctx, worker.UID)
			if firstInterruption == "" {
				firstInterruption = current.OperationID
			}
			if err != nil || held.OperationID != automatic.OperationID || held.State != "unconfirmed" || held.InterruptedBy != firstInterruption {
				t.Fatalf("automatic uncertainty lost: %+v %v", held, err)
			}
			if err := ts.srv.store.FinishActorOperation(ctx, automatic, true); err == nil {
				t.Fatal("late automatic completion released interrupted hold")
			}
			wantStops := 1
			if pve.stops != wantStops {
				t.Fatalf("explicit command stop effects: got %d want %d", pve.stops, wantStops)
			}
		})
	}
}
