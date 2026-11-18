package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/store"
)

func editorUpdate(t *testing.T, ts *substrateServer, swarmID string) updateCaller {
	t.Helper()
	ctx := context.Background()
	swarm, err := ts.srv.store.GetSwarm(ctx, swarmID)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword("editor-password", cheapArgon2)
	if err != nil {
		t.Fatal(err)
	}
	user := store.User{ID: store.NewID(), Username: "update-editor", PasswordHash: hash}
	if err := ts.srv.store.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.AddMember(ctx, swarm.ProjectID, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetSwarmMember(ctx, swarm.ID, user.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	client := newClient(t)
	ts.login(client, user.Username, "editor-password")
	return operatorUpdate(ts, client, swarm.ID)
}

// BUG: Update проверял automatic holds до записи явного owner/actor intent.
// HYPOTHESIS: CheckActorUpdate не различал автоматическую работу и явное управление.
// SOLUTION: Положительные owner/delegated полномочия допускают явный intent;
// его транзакция заново проверяет scope и сохраняет interrupted evidence.
// RESULT: FIXED — полный Go/Python прогон и targeted race проходят.
// STATUS: FIXED
func TestExplicitUpdateInterruptsInflightStep(t *testing.T) {
	ctx := context.Background()
	for _, via := range []string{"operator", "master"} {
		t.Run(via, func(t *testing.T) {
			d := newDispatchServer(t)
			d.exec(`UPDATE managed_actors SET parent_uid=? WHERE uid=?`, d.master.UID, d.worker.UID)
			d.srv.artifactsDir = writeGuestArtifacts(t)
			calls := guestReleaseHub(d.substrateServer, reportsRelease(oldRelease))
			d.start("deliver")
			prior := d.committed()
			if prior.State != "inflight" || prior.Actor != d.worker.UID {
				t.Fatalf("fixture did not dispatch the current step: %+v", prior)
			}
			beforeCalls := len(calls())
			got := updateCallers(d.substrateServer, d.alice, d.master)[via](t, d.worker.Name)
			if !got.ok || got.outcome != "accepted" {
				t.Fatalf("explicit update denied during pipeline step: %+v", got)
			}
			step, err := d.srv.store.GetSwarmStep(ctx, d.swarm, prior.Sequence)
			if err != nil || step.State != "unconfirmed" || step.ActorUID != d.worker.UID || step.RequestID == "" || step.Report != "" || step.Outcome != nil {
				t.Fatalf("update lost original interrupted step or invented a result: %+v %v", step, err)
			}
			current, err := d.srv.store.GetManagedActor(ctx, d.worker.UID)
			if err != nil || current.Stage != "stop" || current.UPID == "" || current.Incarnation != d.worker.Incarnation || current.CTID != d.worker.CTID {
				t.Fatalf("update replaced allocation instead of controlled restart: %+v %v", current, err)
			}
			if afterCalls := calls(); len(afterCalls) != beforeCalls+1 || afterCalls[beforeCalls] != d.worker.UID+" guest_release" {
				t.Fatalf("update did not inspect the exact guest release once: %v", afterCalls[beforeCalls:])
			}
		})
	}
}

func TestExplicitUpdateRetainsLandingToken(t *testing.T) {
	ctx := context.Background()
	ts, alice, master, pve := masterSetup(t)
	ts.srv.artifactsDir = writeGuestArtifacts(t)
	guestReleaseHub(ts, reportsRelease(oldRelease))
	if _, err := ts.srv.managed.executeCoordinationCommand(ctx, master, coordinationInvocation("landing", map[string]any{"action": "take", "request_id": "explicit-update-land"})); err != nil {
		t.Fatal(err)
	}
	swarm, err := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ts.srv.store.ShowLandingToken(ctx, swarm.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	got := operatorUpdate(ts, alice, master.SwarmID)(t, master.Name)
	if !got.ok || got.outcome != "accepted" || pve.stops != 1 {
		t.Fatalf("owner update denied by landing hold: %+v stops=%d", got, pve.stops)
	}
	after, err := ts.srv.store.ShowLandingToken(ctx, swarm.ProjectID)
	if err != nil || after != before {
		t.Fatalf("explicit update released landing authority: %+v %v", after, err)
	}
}

func TestExplicitUpdateInterruptsWorkspaceWithTruthfulOrigin(t *testing.T) {
	ctx := context.Background()
	for _, via := range []string{"operator", "master"} {
		t.Run(via, func(t *testing.T) {
			ts, alice, master, worker, pve, _ := updateSetup(t)
			guestReleaseHub(ts, reportsRelease(oldRelease))
			prior, err := ts.srv.store.AcquireActorOperation(ctx, worker.UID, worker.Incarnation, "automatic-workspace", "automatic", "restart")
			if err != nil {
				t.Fatal(err)
			}
			got := updateCallers(ts, alice, master)[via](t, worker.Name)
			if !got.ok || got.outcome != "accepted" {
				t.Fatalf("explicit %s update blocked by automatic workspace: %+v", via, got)
			}
			current, err := ts.srv.store.GetManagedActor(ctx, worker.UID)
			if err != nil {
				t.Fatal(err)
			}
			origin, err := ts.srv.store.GetOwnerManagedActorOperation(ctx, worker.UID, worker.Incarnation, current.OperationID)
			if err != nil || origin.Operation != "update" || origin.State != "completed" {
				t.Fatalf("update lost positive explicit origin: %+v %v", origin, err)
			}
			if via == "master" && origin.Caller != fmt.Sprintf("actor:%s:%d", master.UID, master.Incarnation) || via == "operator" && !strings.HasPrefix(origin.Caller, "operator:") {
				t.Fatalf("update misattributed explicit caller: %+v", origin)
			}
			held, err := ts.srv.store.CurrentActorOperation(ctx, worker.UID)
			if err != nil || held.OperationID != prior.OperationID || held.State != "unconfirmed" || held.InterruptedBy != current.OperationID {
				t.Fatalf("update erased interrupted workspace evidence: %+v %v", held, err)
			}
			if err := ts.srv.store.CheckActorUpdate(ctx, current); err == nil {
				t.Fatal("explicit update released the automatic coordination hold")
			}
			if pve.stops != 1 || current.CTID != worker.CTID || current.OwnerNonce != worker.OwnerNonce || current.Incarnation != worker.Incarnation || current.Resources != worker.Resources {
				t.Fatalf("update changed allocation or duplicated stop: %+v stops=%d", current, pve.stops)
			}
		})
	}
}
