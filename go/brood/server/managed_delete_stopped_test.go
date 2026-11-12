package server

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// Фейк PVE отклоняет повторный stop без квитанции, как настоящий vzstop:
// HTTP 500 «CT not running», который proxmox.Client отдаёт неоднозначным
// исходом (writeTask), а не отказом до записи.
type stoppedDeletePVE struct {
	*managedFakePVE
	t     *testing.T
	owner proxmox.OwnedCT
	trace []string
}

func (f *stoppedDeletePVE) InspectOwned(ctx context.Context, ref proxmox.OwnedCT) (proxmox.CTConfig, error) {
	f.trace = append(f.trace, "inspect-owned")
	if ref != f.owner {
		f.t.Fatalf("ownership binding changed: got %+v want %+v", ref, f.owner)
	}
	return f.managedFakePVE.InspectOwned(ctx, ref)
}
func (f *stoppedDeletePVE) Status(ctx context.Context, id int) (proxmox.CTStatus, error) {
	f.trace = append(f.trace, "status")
	return f.managedFakePVE.Status(ctx, id)
}
func (f *stoppedDeletePVE) Stop(ctx context.Context, ref proxmox.OwnedCT) (string, error) {
	f.trace = append(f.trace, "stop")
	if _, err := f.InspectOwned(ctx, ref); err != nil {
		return "", err
	}
	f.mu.Lock()
	running := f.running
	f.mu.Unlock()
	if !running {
		return "", fmt.Errorf("%w: PVE HTTP status 500", proxmox.ErrUncertain)
	}
	return f.managedFakePVE.Stop(ctx, ref)
}
func (f *stoppedDeletePVE) Delete(ctx context.Context, ref proxmox.OwnedCT) (string, error) {
	f.trace = append(f.trace, "delete")
	if _, err := f.InspectOwned(ctx, ref); err != nil {
		return "", err
	}
	a, err := f.server.store.GetManagedActor(ctx, ref.Owner.ActorUID)
	if err != nil || a.Stage != "delete" || a.Operation != "delete" || a.Desired != "deleted" || a.UPID != "" {
		f.t.Fatalf("delete without durable fresh delete slot: %+v %v", a, err)
	}
	return f.managedFakePVE.Delete(ctx, ref)
}
func (f *stoppedDeletePVE) WaitTask(ctx context.Context, upid string) error {
	f.trace = append(f.trace, "wait:"+upid)
	return f.managedFakePVE.WaitTask(ctx, upid)
}
func (f *stoppedDeletePVE) ConfirmAbsent(ctx context.Context, id int) error {
	f.trace = append(f.trace, "global-absence")
	return f.managedFakePVE.ConfirmAbsent(ctx, id)
}

func TestManagedDeleteStoppedDoesNotResubmitStop(t *testing.T) {
	for _, scenario := range []string{"normal", "foreign owner", "unknown prior submission"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			ts, alice, _, id, pve, hub := managedSetup(t)
			path := "/api/swarms/" + id + "/actors"
			r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "stopped-delete"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
			expectStatus(t, "create", r, b, 201)
			hub.connected = true
			for range 3 {
				ts.srv.managed.reconcileOnce(ctx)
			}
			before := mustManagedActor(t, ts, id)
			strict := &stoppedDeletePVE{managedFakePVE: pve, t: t, owner: managedOwned(before)}
			ts.srv.managed.clients[before.PoolUID] = strict
			path += "/stopped-delete"
			r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), map[string]string{"action": "stop"})
			expectStatus(t, "stop", r, b, 200)
			ts.srv.managed.reconcileOnce(ctx)
			stopped := mustManagedActor(t, ts, id)
			if stopped.Stage != "done" || stopped.Phase != "stopped" || stopped.UPID != "stop-task" || pve.stops != 1 {
				t.Fatalf("stop not completed: %+v", stopped)
			}
			t.Logf("stop API trace: %v", strict.trace)
			strict.trace = nil

			switch scenario {
			case "foreign owner":
				pve.spec.Owner.Nonce = "foreign-owner"
			case "unknown prior submission":
				// Воспроизводим старую небезопасную стадию: повторный stop
				// получил отказ без UPID. Автоматика не вправе его согласовать.
				a, err := ts.srv.store.BeginManagedActorOperation(ctx, stopped.UID, stopped.Revision, store.NewID(), "delete")
				if err != nil {
					t.Fatal(err)
				}
				a, err = ts.srv.managed.launch(ctx, strict, a)
				if err != nil || a.Stage != "stop" || a.UPID != "" || a.Phase != "error" {
					t.Fatalf("expected retained rejected stop: %+v %v", a, err)
				}
				t.Logf("unsafe fixture trace: %v", strict.trace)
				strict.trace = nil
			}
			r, b, _ = ts.do(alice, "DELETE", path, ts.csrf(alice), nil)
			if scenario == "foreign owner" {
				expectStatus(t, "ownership refusal", r, b, 409)
				a := mustManagedActor(t, ts, id)
				if a != stopped || pve.deletes != 0 || !reflect.DeepEqual(strict.trace, []string{"inspect-owned", "global-absence"}) {
					t.Fatalf("foreign owner changed allocation: %+v trace=%v", a, strict.trace)
				}
				return
			}
			if scenario == "unknown prior submission" {
				expectStatus(t, "retain unknown delete", r, b, 202)
				for range 3 {
					ts.srv.managed.reconcileOnce(ctx)
				}
				a := mustManagedActor(t, ts, id)
				if a.Stage != "stop" || a.UPID != "" || a.Desired != "deleted" || a.Phase == "deleted" || !pve.exists || pve.deletes != 0 || len(strict.trace) != 0 || managedOwned(a) != strict.owner {
					t.Fatalf("unknown submission bypassed: %+v trace=%v", a, strict.trace)
				}
				return
			}
			t.Logf("delete API trace: %v", strict.trace)
			expectStatus(t, "delete stopped", r, b, 202)
			a := mustManagedActor(t, ts, id)
			if a.Stage != "delete" || a.UPID != "delete-task" || a.Phase == "deleted" || pve.stops != 1 || pve.deletes != 1 || managedOwned(a) != strict.owner {
				t.Fatalf("delete did not submit exactly once without stop: %+v trace=%v", a, strict.trace)
			}
			pve.absenceError = proxmox.ErrUncertain
			ts.srv.managed.reconcileOnce(ctx)
			if a = mustManagedActor(t, ts, id); a.Phase == "deleted" {
				t.Fatal("failed global absence released allocation")
			}
			pve.absenceError = nil
			ts.srv.managed.reconcileOnce(ctx)
			a = mustManagedActor(t, ts, id)
			want := []string{"inspect-owned", "status", "delete", "inspect-owned", "wait:delete-task", "global-absence", "wait:delete-task", "global-absence"}
			if a.Phase != "deleted" || a.Stage != "done" || a.UPID != "delete-task" || managedOwned(a) != strict.owner || pve.deletes != 1 || !reflect.DeepEqual(strict.trace, want) {
				t.Fatalf("cleanup proof incomplete: %+v trace=%v want=%v", a, strict.trace, want)
			}
			t.Logf("completed delete trace: %v", strict.trace)
		})
	}
}
