package server

import (
	"context"
	"reflect"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

func completeEphemeralCleanup(d *dispatchServer, intent store.EphemeralChildIntent, actor store.ManagedActor) (store.ManagedActor, error) {
	return d.srv.completeEphemeralChildCleanup(context.Background(), d.master.DelegatorID, intent.Parent, actor.UID, actor.Incarnation, actor.OperationID, actor.Revision)
}

type cleanupAbsenceHookPVE struct {
	proxmoxAPI
	after func(context.Context) error
}

func (p *cleanupAbsenceHookPVE) ConfirmAbsent(ctx context.Context, id int) error {
	if err := p.proxmoxAPI.ConfirmAbsent(ctx, id); err != nil {
		return err
	}
	return p.after(ctx)
}

// FEATURE: Только отдельный finalizer освобождает автоматическую reservation.
// SPEC: Positive global absence + успешная terminal task либо proven unrequested stage,
// исходные scope/CAS и FULL commit; unknown не становится permission от absence.
// IMPLEMENTATION: После внешних proof/read текущий scope повторяется в FULL tx;
// stop-close/deleted атомарны, retained readback ловит ignored writes.
// RESULT: IMPLEMENTED — success/failure cleanup, девять границ и полный Go/Python
// прогон с focused race проходят без provider writes в finalizer.
// STATUS: DONE
func TestEphemeralCleanupFinalizationRequiresPositiveProofAndCurrentScope(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"delete", "unsent-delete", "unsent-stop", "unknown-delete", "pending-delete", "foreign-id", "revoked", "revoked-after-proof", "ignored-final-write"} {
		t.Run(scenario, func(t *testing.T) {
			stage := "delete"
			if scenario == "unsent-stop" || scenario == "ignored-final-write" {
				stage = "stop"
			}
			d, intent, actor, pve := lifecycleServerFixture(t, stage)
			if scenario == "unknown-delete" {
				pve.lost = true
			}
			if scenario != "unsent-delete" && scenario != "unsent-stop" && scenario != "ignored-final-write" {
				var err error
				actor, err = lifecycleSubmit(d, intent, actor)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				pve.mu.Lock()
				pve.exists = false
				pve.running = false
				pve.mu.Unlock()
			}
			switch scenario {
			case "pending-delete":
				pve.taskErrors = map[string]error{"delete-task": context.DeadlineExceeded}
			case "foreign-id":
				pve.mu.Lock()
				pve.exists = true
				pve.spec.Owner.Nonce = "foreign-owner"
				pve.mu.Unlock()
			case "revoked":
				if err := d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true); err != nil {
					t.Fatal(err)
				}
			case "revoked-after-proof":
				provider := &cleanupAbsenceHookPVE{proxmoxAPI: pve, after: func(ctx context.Context) error {
					return d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true)
				}}
				d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return provider, nil }
				delete(d.srv.managed.clients, actor.PoolUID)
			case "ignored-final-write":
				createSQL(t, d, `CREATE TRIGGER ignored_finalization BEFORE UPDATE OF phase ON managed_actors WHEN NEW.phase='deleted' BEGIN SELECT RAISE(IGNORE); END`)
			}
			createSQL(t, d, `CREATE TRIGGER finalization_full BEFORE UPDATE OF phase ON managed_actors WHEN NEW.phase='deleted' AND (SELECT synchronous FROM pragma_synchronous)<>2 BEGIN SELECT RAISE(ABORT,'final absence release requires FULL'); END`)
			beforeEffects := pve.calls.Load()
			beforeAbsence := pve.absenceChecks
			out, err := completeEphemeralCleanup(d, intent, actor)
			expectSuccess := scenario == "delete" || scenario == "unsent-delete" || scenario == "unsent-stop"
			current, readErr := d.srv.store.GetManagedActor(ctx, actor.UID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !expectSuccess {
				if err == nil || current != actor || pve.calls.Load() != beforeEffects {
					t.Fatalf("unsafe finalization changed the held reservation: %+v effects=%d %v", current, pve.calls.Load(), err)
				}
				if (scenario == "unknown-delete" || scenario == "pending-delete" || scenario == "revoked") && pve.absenceChecks != beforeAbsence {
					t.Fatal("unknown task or revoked authority reached global absence permission")
				}
				return
			}
			if err != nil || !reflect.DeepEqual(out, current) || current.Phase != "deleted" || current.Stage != "done" || current.DeletedAt == nil || current.Incarnation != actor.Incarnation || current.OwnerNonce != actor.OwnerNonce || current.CTID != actor.CTID || current.OperationID != actor.OperationID {
				t.Fatalf("positive proof did not atomically release the exact reservation: %+v %v", out, err)
			}
			if pve.calls.Load() != beforeEffects || pve.absenceChecks != beforeAbsence+1 {
				t.Fatalf("finalizer submitted an effect or omitted exact global proof: effects=%d absence=%d", pve.calls.Load(), pve.absenceChecks)
			}
			seal, err := d.srv.store.GetEphemeralChildResultSeal(ctx, intent.ID)
			if err != nil || seal.ChildID == "" {
				t.Fatalf("finalization changed independent result retention: %+v %v", seal, err)
			}
		})
	}
}
