package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// FEATURE: Узкий producer логического отказа до регистрации executor.
// SPEC: Только bound terminal failure исходного Create/первого automatic Start;
// observer не пишет failure, coordinator сохраняет receipt и physical reservation.
// IMPLEMENTATION: Отдельный producer использует local callback в FULL transaction;
// точный snapshot и provenance проверяются после namespace lookup и owner gates.
// RESULT: IMPLEMENTED — behavioral RED→GREEN; focused normal/race, build/vet прошли.
// STATUS: DONE
func failureDetectionFixture(t *testing.T, operation string) (*dispatchServer, store.EphemeralChildIntent, store.ManagedActor, *managedFakePVE) {
	t.Helper()
	ctx := context.Background()
	var d *dispatchServer
	var intent store.EphemeralChildIntent
	var actor store.ManagedActor
	var fake *managedFakePVE
	var err error
	if operation == "create" {
		var provider *ephemeralCreatePVE
		d, intent, actor, provider = ephemeralCreateFixture(t)
		actor, err = d.srv.createAllocatedEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
		fake = provider.managedFakePVE
	} else {
		var provider *lifecyclePVE
		d, intent, actor, provider = lifecycleServerFixture(t, "start")
		actor, err = lifecycleSubmit(d, intent, actor)
		fake = provider.managedFakePVE
	}
	if err != nil {
		t.Fatal(err)
	}
	addAllocatedControllerSibling(t, d, intent, true)
	return d, intent, actor, fake
}

func observeFailureDetection(t *testing.T, d *dispatchServer, actor store.ManagedActor) store.ManagedActor {
	t.Helper()
	d.srv.managed.lifecycle.Lock()
	d.srv.managed.reconcilePass(context.Background(), actor)
	d.srv.managed.lifecycle.Unlock()
	current, err := d.srv.store.GetManagedActor(context.Background(), actor.UID)
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func TestEphemeralFailureDetectionBoundTerminalTask(t *testing.T) {
	for _, operation := range []string{"create", "start"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			d, intent, actor, fake := failureDetectionFixture(t, operation)
			fake.taskErrors = map[string]error{actor.UPID: proxmox.ErrTaskFailed}
			terminal := observeFailureDetection(t, d, actor)
			if terminal.Stage != "done" || terminal.Phase != "error" || terminal.UPID == "" || terminal.Error != managedProviderError(proxmox.ErrTaskFailed) {
				t.Fatalf("observer did not retain positive terminal evidence: %+v", terminal)
			}
			if _, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("observer wrote logical failure: %v", err)
			}
			before := providerEffectsOf(fake)
			allocation, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			createSQL(t, d, `CREATE TRIGGER failure_detection_full BEFORE INSERT ON ephemeral_child_failures WHEN (SELECT synchronous FROM pragma_synchronous)<>2 BEGIN SELECT RAISE(ABORT,'failure writer requires FULL'); END`)
			if err := advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
				t.Fatal(err)
			}
			failure, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID)
			if err != nil || failure.RequestID != intent.RequestID || failure.Reason != "Original worker provisioning task failed before child registration." || failure.FailedAt == "" {
				t.Fatalf("controller did not record trusted bound %s task failure: %+v %v", operation, failure, err)
			}
			current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			retained, allocationErr := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID)
			if err != nil || allocationErr != nil || current != terminal || retained != allocation || providerEffectsOf(fake) != before {
				t.Fatalf("producer changed receipt, allocation or provider effects: %+v %v/%v", current, err, allocationErr)
			}
			if _, err := d.srv.store.GetEphemeralChildResultSeal(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("producer invented result seal: %v", err)
			}
			job, err := d.srv.allocatedEphemeralGroup(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.srv.recordAllocatedEphemeralTaskFailure(ctx, d.master.DelegatorID, job, terminal); err != nil {
				t.Fatalf("exact retry refused: %v", err)
			}
			// Повтор coordinator до aggregate join не вправе начать cleanup.
			if err := advanceAllocatedChildForTest(t, d, intent.ID); err == nil {
				t.Fatal("cleanup before applied aggregate join was not refused")
			}
			again, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID)
			current, actorErr := d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil || actorErr != nil || again != failure || current != terminal || providerEffectsOf(fake) != before {
				t.Fatal("retry changed logical failure or began early cleanup")
			}
			jobs, err := d.srv.store.EphemeralChildHistoryAtStep(ctx, intent.Parent.Principal.SwarmID, intent.Parent.Sequence)
			if err != nil {
				t.Fatal(err)
			}
			for _, sibling := range jobs {
				if sibling.Intent.ID != intent.ID {
					if err := d.srv.store.RecordEphemeralChildFailure(ctx, sibling.Intent.ID, "Explicit trusted sibling failure", d.srv.managed.childAuthority); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := d.srv.store.JoinSwarmChildren(ctx, intent.Parent, d.srv.managed.childAuthority); err != nil {
				t.Fatal(err)
			}
			if err := advanceAllocatedChildForTest(t, d, intent.ID); err != nil {
				t.Fatal(err)
			}
			current, err = d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil || current.Operation != "delete" || current.Stage != "stop" || !stopNotRequested(current) || providerEffectsOf(fake) != before {
				t.Fatalf("applied failed join did not begin only a separate unsent cleanup: %+v %v", current, err)
			}
		})
	}
}

func TestEphemeralFailureDetectionHoldsUncertainObservations(t *testing.T) {
	for _, operation := range []string{"create", "start"} {
		for _, tc := range []struct {
			name      string
			taskError error
		}{
			{"unknown", proxmox.ErrUncertain}, {"transient", proxmox.ErrPermission}, {"pending", context.DeadlineExceeded},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				d, intent, actor, fake := failureDetectionFixture(t, operation)
				fake.taskErrors = map[string]error{actor.UPID: tc.taskError}
				observed := observeFailureDetection(t, d, actor)
				before := providerEffectsOf(fake)
				_ = advanceAllocatedChildForTest(t, d, intent.ID)
				if _, err := d.srv.store.GetEphemeralChildFailure(context.Background(), intent.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("uncertainty became logical failure: %v", err)
				}
				current, err := d.srv.store.GetManagedActor(context.Background(), actor.UID)
				if err != nil || current != observed || providerEffectsOf(fake) != before {
					t.Fatalf("held task changed: %+v %v", current, err)
				}
			})
		}
	}
}

func TestEphemeralFailureDetectionRequiresCompleteReceiptAndProvenance(t *testing.T) {
	for _, operation := range []string{"create", "start"} {
		for _, hold := range []string{"no-upid", "nonterminal", "diagnostic", "wrong-operation", "unconfirmed-parent", "unconfirmed-executor", "pool-revoked", "pool-source"} {
			t.Run(operation+"/"+hold, func(t *testing.T) {
				ctx := context.Background()
				d, intent, actor, fake := failureDetectionFixture(t, operation)
				fake.taskErrors = map[string]error{actor.UPID: proxmox.ErrTaskFailed}
				terminal := observeFailureDetection(t, d, actor)
				// Seed неполного/старого receipt не создаёт positive task authority.
				switch hold {
				case "no-upid":
					createSQL(t, d, `UPDATE managed_actors SET upid='' WHERE uid=?`, actor.UID)
				case "nonterminal":
					createSQL(t, d, `UPDATE managed_actors SET stage=? WHERE uid=?`, operation, actor.UID)
				case "diagnostic":
					terminal.Error = "Provider returned an unexpected error."
					if _, err := d.srv.store.UpdateManagedActor(ctx, terminal, terminal.Revision); err != nil {
						t.Fatal(err)
					}
				case "wrong-operation":
					createSQL(t, d, `UPDATE managed_actors SET operation_id='unproven-replacement' WHERE uid=?`, actor.UID)
				case "pool-revoked":
					createSQL(t, d, `UPDATE managed_pools SET state='draining' WHERE uid=?`, actor.PoolUID)
				case "pool-source":
					createSQL(t, d, `UPDATE managed_pools SET source_fingerprint='replacement' WHERE uid=?`, actor.PoolUID)
				default:
					uid, inc := actor.UID, actor.Incarnation
					if hold == "unconfirmed-parent" {
						uid, inc = d.master.UID, d.master.Incarnation
					}
					op := store.ActorOperation{ProjectID: intent.Parent.Principal.ProjectID, SwarmID: d.swarm, UID: uid, Generation: inc, OperationID: "unconfirmed-workspace", Caller: "trusted", Operation: "restart", State: "unconfirmed"}
					payload, err := json.Marshal(op)
					if err != nil {
						t.Fatal(err)
					}
					createSQL(t, d, `INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, string(payload), op.UID)
				}
				held, err := d.srv.store.GetManagedActor(ctx, actor.UID)
				if err != nil {
					t.Fatal(err)
				}
				before := providerEffectsOf(fake)
				_ = advanceAllocatedChildForTest(t, d, intent.ID)
				if _, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("incomplete evidence wrote logical failure: %v", err)
				}
				current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
				if err != nil || current != held || providerEffectsOf(fake) != before {
					t.Fatalf("held receipt changed: %+v %v", current, err)
				}
			})
		}
	}
}

func TestEphemeralFailureDetectionRechecksAfterExternalNamespace(t *testing.T) {
	for _, change := range []string{"actor", "owner", "source", "pool", "namespace"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			d, intent, actor, fake := failureDetectionFixture(t, "start")
			fake.taskErrors = map[string]error{actor.UPID: proxmox.ErrTaskFailed}
			terminal := observeFailureDetection(t, d, actor)
			checks := 0
			d.srv.ateapi = nil
			d.srv.provision = &probingProvisioner{uid: actor.AtespaceUID, probe: func() error {
				checks++
				// Отдельный Store read блокировался бы при namespace lookup внутри tx.
				probeCtx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				if _, err := d.srv.store.GetSwarm(probeCtx, d.swarm); err != nil {
					return err
				}
				switch change {
				case "actor":
					terminal.Error = managedProviderError(proxmox.ErrUncertain)
					_, err := d.srv.store.UpdateManagedActor(ctx, terminal, terminal.Revision)
					return err
				case "owner":
					return d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true)
				case "source":
					return d.srv.store.SetProjectRepository(ctx, intent.Parent.Principal.ProjectID, "https://github.com/other/repo.git")
				case "pool":
					createSQL(t, d, `UPDATE managed_pools SET state='draining' WHERE uid=?`, actor.PoolUID)
				case "namespace":
					createSQL(t, d, `UPDATE swarms SET atespace_uid='replacement' WHERE id=?`, d.swarm)
				}
				return nil
			}}
			before := providerEffectsOf(fake)
			if err := advanceAllocatedChildForTest(t, d, intent.ID); err == nil {
				t.Fatal("changed current authority or snapshot was accepted")
			}
			if checks != 1 {
				t.Fatalf("namespace lookup count = %d", checks)
			}
			if _, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("stale preflight wrote failure: %v", err)
			}
			if providerEffectsOf(fake) != before {
				t.Fatal("producer called provider")
			}
		})
	}
}

func TestEphemeralFailureDetectionManualOrigin(t *testing.T) {
	ctx := context.Background()
	d, intent, actor, fake := failureDetectionFixture(t, "create")
	fake.taskErrors = map[string]error{actor.UPID: proxmox.ErrTaskFailed}
	actor = observeFailureDetection(t, d, actor)
	actor, err := d.srv.store.BeginOwnerManagedActorOperation(ctx, actor, "manual-start", "start", d.master.DelegatorID, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	actor, err = d.srv.store.BindManagedActorTask(ctx, actor.UID, actor.Revision, actor.OperationID, "start", "manual-start-task")
	if err != nil {
		t.Fatal(err)
	}
	fake.taskErrors = map[string]error{actor.UPID: proxmox.ErrTaskFailed}
	actor = observeFailureDetection(t, d, actor)
	if actor.Stage != "done" || actor.Phase != "error" || actor.UPID != "manual-start-task" {
		t.Fatalf("manual task did not fail: %+v", actor)
	}
	if _, err := d.srv.ownerActorOperationOrigin(ctx, actor); err != nil {
		t.Fatal(err)
	}
	before := providerEffectsOf(fake)
	_ = advanceAllocatedChildForTest(t, d, intent.ID)
	if _, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("manual operation became automatic failure: %v", err)
	}
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current != actor || providerEffectsOf(fake) != before {
		t.Fatalf("manual task changed: %+v %v", current, err)
	}
}

func TestEphemeralFailureDetectionDoesNotClassifyRegisteredTask(t *testing.T) {
	ctx := context.Background()
	d, actor, child := runningEphemeralChildReportFixture(t)
	intent, err := d.srv.store.GetEphemeralChildIntentForTask(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Даже похожая диагностика не даёт producer право классифицировать child task.
	actor.Phase, actor.Error = "error", managedProviderError(proxmox.ErrTaskFailed)
	actor, err = d.srv.store.UpdateManagedActor(ctx, actor, actor.Revision)
	if err != nil {
		t.Fatal(err)
	}
	_ = advanceAllocatedChildForTest(t, d, intent.ID)
	if _, err := d.srv.store.GetEphemeralChildFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("registered task got pre-registration failure: %v", err)
	}
	work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil || work.Task != child {
		t.Fatalf("registered task/report changed: %+v %v", work.Task, err)
	}
	current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
	if err != nil || current != actor {
		t.Fatalf("registered actor changed: %+v %v", current, err)
	}
}

func TestEphemeralFailureDetectionRevokedTerminalTask(t *testing.T) {
	for _, operation := range []string{"create", "start"} {
		t.Run(operation, func(t *testing.T) {
			d, intent, actor, fake := failureDetectionFixture(t, operation)
			fake.taskErrors = map[string]error{actor.UPID: proxmox.ErrTaskFailed}
			terminal := observeFailureDetection(t, d, actor)
			if err := d.srv.store.SetUserDisabled(context.Background(), d.master.DelegatorID, true); err != nil {
				t.Fatal(err)
			}
			_ = advanceAllocatedChildForTest(t, d, intent.ID)
			if _, err := d.srv.store.GetEphemeralChildFailure(context.Background(), intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("revoked authority wrote failure: %v", err)
			}
			current, err := d.srv.store.GetManagedActor(context.Background(), actor.UID)
			if err != nil || current != terminal {
				t.Fatalf("revocation changed actor: %+v %v", current, err)
			}
		})
	}
}
