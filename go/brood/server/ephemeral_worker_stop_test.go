package server

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// FEATURE: Registered automatic workers fail only on positive owned container stop (#292).
// SPEC: Scheduled recovery retains child/report/delivery and capacity, requires original
// automatic Start lineage, and never treats missing sessions or uncertain reads as death.
// IMPLEMENTATION: Allocated recovery observes exact owned CT state under lifecycle/effect
// gates, then records a separate FULL executor failure with local current-scope rechecks.
// RESULT: IMPLEMENTED — behavioral RED→GREEN; focused normal/race, full Go tests/build/vet pass.
// STATUS: DONE
func observedWorkerFixture(t *testing.T, running bool) (*dispatchServer, store.EphemeralChildIntent, store.ManagedActor, store.SwarmChildTask, *managedFakePVE) {
	t.Helper()
	ctx := context.Background()
	d, intent, actor, provider := lifecycleServerFixture(t, "start")
	var err error
	actor, err = lifecycleSubmit(d, intent, actor)
	if err != nil {
		t.Fatal(err)
	}
	actor, err = d.srv.store.ConfirmManagedActorStage(ctx, actor.UID, actor.Revision, actor.OperationID, "start", "running")
	if err != nil {
		t.Fatal(err)
	}
	connected := true
	d.onConnected(func(base commandSessionHub, uid string, inc int64) bool {
		if uid == actor.UID {
			return connected && inc == actor.Incarnation
		}
		return base.Connected(uid, inc)
	})
	if _, err = d.srv.attestEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID, "observed-stop-proof"); err != nil {
		t.Fatal(err)
	}
	child, err := d.srv.registerEphemeralChild(ctx, d.master.DelegatorID, intent.Parent, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running {
		allocation, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := d.srv.store.GetEphemeralChildAttestation(ctx, intent.ID)
		if err != nil {
			t.Fatal(err)
		}
		body, err := ephemeralChildInstruction(ephemeralInstructionSource{Intent: intent, Allocation: allocation, Proof: proof, Actor: actor, Work: store.SwarmChildWork{Parent: intent.Parent, Task: child}})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(relay.MessageRequest{To: actor.Name, From: stepMessageSender(d.swarm), Body: body})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.srv.store.PrepareEphemeralChildDelivery(ctx, intent.ID, store.ActorDeliveryInput{SwarmID: d.swarm, SourceKind: "child", SourceID: child.ID, RequestID: intent.RequestID, Commit: intent.SourceCommit, ActorUID: actor.UID, Incarnation: actor.Incarnation, AtespaceUID: actor.AtespaceUID, JournalID: strings.Repeat("d", 64), Payload: string(payload)}, d.srv.managed.childAuthority); err != nil {
			t.Fatal(err)
		}
		work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
		if err != nil {
			t.Fatal(err)
		}
		child = work.Task
	}
	addAllocatedControllerSibling(t, d, intent, true)
	connected = false
	return d, intent, actor, child, provider.managedFakePVE
}

func TestObservedWorkerStopRecovery(t *testing.T) {
	for _, running := range []bool{false, true} {
		name := "pending"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			d, intent, actor, child, fake := observedWorkerFixture(t, running)
			fake.running = false
			before := providerEffectsOf(fake)
			calls, messages := d.calls()
			allocation, err := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			createSQL(t, d, `CREATE TRIGGER worker_failure_full BEFORE INSERT ON ephemeral_child_executor_failures WHEN (SELECT synchronous FROM pragma_synchronous)<>2 BEGIN SELECT RAISE(ABORT,'worker failure requires FULL'); END`)
			_, _ = recoverAllocatedForTest(t, d, ctx, 0, 1)
			failure, err := d.srv.store.GetEphemeralChildExecutorFailure(ctx, intent.ID)
			if err != nil || failure.ChildID != child.ID || failure.RequestID != child.RequestID || failure.Reason != "Registered worker container was observed stopped with verified ownership." || failure.FailedAt == "" {
				t.Fatalf("scheduled recovery did not retain positive worker stop: %+v %v", failure, err)
			}
			_, _ = recoverAllocatedForTest(t, d, ctx, 0, 1)
			current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			retained, allocErr := d.srv.store.GetEphemeralChildAllocation(ctx, intent.ID)
			work, workErr := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
			afterCalls, afterMessages := d.calls()
			if err != nil || allocErr != nil || workErr != nil || current != actor || retained != allocation || work.Task != child || providerEffectsOf(fake) != before || len(afterCalls) != len(calls) || len(afterMessages) != len(messages) {
				t.Fatal("failure fabricated child result, changed physical hold, dispatched instructions or began early cleanup")
			}
			if _, err := d.srv.store.GetEphemeralChildResultSeal(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("invented result seal: %v", err)
			}
		})
	}
}

// workerObservationPVE checks exact physical coordinates and permits a local
// authority change after observation, before the durable failure callback.
type workerObservationPVE struct {
	*managedFakePVE
	t                   *testing.T
	actor               store.ManagedActor
	status              string
	statusErr           error
	afterStatus         func(context.Context)
	inspected, statuses int
}

func (p *workerObservationPVE) InspectOwned(ctx context.Context, ref proxmox.OwnedCT) (proxmox.CTConfig, error) {
	p.inspected++
	if ref != managedOwned(p.actor) {
		p.t.Fatalf("foreign observation target: %+v", ref)
	}
	// Separate Store access would time out if called inside a Store callback.
	probe, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := p.server.store.GetSwarm(probe, p.actor.SwarmID); err != nil {
		p.t.Fatalf("provider observation inside transaction: %v", err)
	}
	return p.managedFakePVE.InspectOwned(ctx, ref)
}

func (p *workerObservationPVE) Status(ctx context.Context, id int) (proxmox.CTStatus, error) {
	p.statuses++
	if id != p.actor.CTID {
		p.t.Fatalf("foreign CTID: %d", id)
	}
	if p.afterStatus != nil {
		p.afterStatus(ctx)
	}
	return proxmox.CTStatus{Status: p.status}, p.statusErr
}

func installWorkerObservation(t *testing.T, d *dispatchServer, actor store.ManagedActor, fake *managedFakePVE) *workerObservationPVE {
	t.Helper()
	p := &workerObservationPVE{managedFakePVE: fake, t: t, actor: actor, status: "stopped"}
	delete(d.srv.managed.clients, actor.PoolUID)
	d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return p, nil }
	return p
}

func TestObservedWorkerStopRechecksAfterProvider(t *testing.T) {
	for _, change := range []string{"actor", "incarnation", "owner", "source", "pool", "namespace", "child", "executor-intervention", "parent-intervention", "executor-operation", "parent-operation"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			d, intent, actor, child, fake := observedWorkerFixture(t, true)
			p := installWorkerObservation(t, d, actor, fake)
			p.afterStatus = func(ctx context.Context) {
				switch change {
				case "actor":
					createSQL(t, d, `UPDATE managed_actors SET owner_nonce='replacement' WHERE uid=?`, actor.UID)
				case "incarnation":
					createSQL(t, d, `UPDATE managed_actors SET incarnation=2 WHERE uid=?`, actor.UID)
				case "owner":
					if err := d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true); err != nil {
						t.Fatal(err)
					}
				case "source":
					if err := d.srv.store.SetProjectRepository(ctx, intent.Parent.Principal.ProjectID, "https://github.com/other/repo.git"); err != nil {
						t.Fatal(err)
					}
				case "pool":
					createSQL(t, d, `UPDATE managed_pools SET state='draining' WHERE uid=?`, actor.PoolUID)
				case "namespace":
					createSQL(t, d, `UPDATE swarms SET atespace_uid='replacement' WHERE id=?`, d.swarm)
				case "child":
					if _, err := d.srv.store.CompleteSwarmChild(ctx, intent.Parent, child.RequestID, child.Executor, child.Revision, "success", "Independent concurrent report", d.srv.managed.childAuthority); err != nil {
						t.Fatal(err)
					}
				case "executor-intervention", "parent-intervention":
					target := actor
					if change == "parent-intervention" {
						target = d.master
					}
					op, err := d.srv.store.AcquireOwnerActorOperation(ctx, target, "worker-stop-control", "operator:"+d.master.DelegatorID, "session_terminal", d.master.DelegatorID, func(context.Context) error { return nil })
					if err != nil {
						t.Fatal(err)
					}
					if err := d.srv.store.FinishActorOperation(ctx, op, true); err != nil {
						t.Fatal(err)
					}
				case "executor-operation", "parent-operation":
					target := actor
					if change == "parent-operation" {
						target = d.master
					}
					op := store.ActorOperation{ProjectID: intent.Parent.Principal.ProjectID, SwarmID: d.swarm, UID: target.UID, Generation: target.Incarnation, OperationID: "unknown-workspace", Caller: "trusted", Operation: "restart", State: "unconfirmed"}
					payload, err := json.Marshal(op)
					if err != nil {
						t.Fatal(err)
					}
					createSQL(t, d, `INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES(?,'workspace',?,?,?)`, op.ProjectID, op.OperationID, string(payload), op.UID)
				}
			}
			before := providerEffectsOf(fake)
			_, err := recoverAllocatedForTest(t, d, ctx, 0, 1)
			if err == nil || p.inspected != 1 || p.statuses != 1 {
				t.Fatalf("stale current scope accepted or provider not reached: %d/%d %v", p.inspected, p.statuses, err)
			}
			if _, err := d.srv.store.GetEphemeralChildExecutorFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("stale observation recorded failure: %v", err)
			}
			if providerEffectsOf(fake) != before {
				t.Fatal("observation mutated provider")
			}
		})
	}
}

func TestObservedWorkerStopUnknownStatusAndDeliveryHeld(t *testing.T) {
	for _, scenario := range []string{"unknown-status", "status-error", "deadline", "unknown-delivery", "not-sent-delivery"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			d, intent, actor, child, fake := observedWorkerFixture(t, true)
			p := installWorkerObservation(t, d, actor, fake)
			p.status = "unknown"
			switch scenario {
			case "status-error":
				p.statusErr = proxmox.ErrPermission
			case "deadline":
				p.statusErr = context.DeadlineExceeded
			case "unknown-delivery", "not-sent-delivery":
				p.status = "running"
				delivery, err := d.srv.store.GetActorDeliveryForSource(ctx, d.swarm, "child", child.ID)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				ticket, err := d.srv.store.ReserveActorDeliveryAttempt(ctx, delivery.ID, now, now.Add(time.Minute), func(context.Context) bool { return true })
				if err != nil {
					t.Fatal(err)
				}
				outcome := "unconfirmed"
				if scenario == "not-sent-delivery" {
					outcome = "not_sent"
				}
				if err := d.srv.store.RecordActorDeliveryOutcome(ctx, ticket, outcome, "Synthetic uncertain transport", now, func(context.Context) bool { return true }); err != nil {
					t.Fatal(err)
				}
			}
			before, err := d.srv.store.GetActorDeliveryForSource(ctx, d.swarm, "child", child.ID)
			if err != nil {
				t.Fatal(err)
			}
			effects := providerEffectsOf(fake)
			_, _ = recoverAllocatedForTest(t, d, ctx, 0, 1)
			after, err := d.srv.store.GetActorDeliveryForSource(ctx, d.swarm, "child", child.ID)
			if err != nil || !reflect.DeepEqual(before, after) || effects != providerEffectsOf(fake) {
				t.Fatal("held delivery retried or changed")
			}
			if _, err := d.srv.store.GetEphemeralChildExecutorFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("uncertainty became failure: %v", err)
			}
		})
	}
}

func TestObservedWorkerStopAppliedJoinCleanup(t *testing.T) {
	for _, unknownDelete := range []bool{false, true} {
		name := "confirmed-delete"
		if unknownDelete {
			name = "unknown-delete"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			d, intent, actor, child, fake := observedWorkerFixture(t, true)
			fake.running = false
			before := providerEffectsOf(fake)
			if _, err := recoverAllocatedForTest(t, d, ctx, 0, 1); err != nil {
				t.Fatal(err)
			}
			// Даже ready_fail не даёт cleanup до применения aggregate join.
			jobs, err := d.srv.store.EphemeralChildHistoryAtStep(ctx, d.swarm, intent.Parent.Sequence)
			if err != nil {
				t.Fatal(err)
			}
			for _, job := range jobs {
				if job.Intent.ID != intent.ID {
					if err := d.srv.store.RecordEphemeralChildFailure(ctx, job.Intent.ID, "Explicit trusted sibling failure", d.srv.managed.childAuthority); err != nil {
						t.Fatal(err)
					}
				}
			}
			ready, err := d.srv.store.GetSwarmChildJoin(ctx, intent.Parent, d.srv.managed.childAuthority)
			if err != nil || ready.Evaluation.State != "ready_fail" {
				t.Fatalf("not ready for aggregate failure: %+v %v", ready, err)
			}
			if _, err := recoverAllocatedForTest(t, d, ctx, 0, 1); err == nil {
				t.Fatal("unapplied aggregate permitted cleanup")
			}
			current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil || current != actor || providerEffectsOf(fake) != before {
				t.Fatal("early cleanup changed physical state")
			}
			joined, err := d.srv.store.JoinSwarmChildren(ctx, intent.Parent, d.srv.managed.childAuthority)
			if err != nil || joined.Evaluation.State != "ready_fail" {
				t.Fatalf("failure join refused: %+v %v", joined, err)
			}
			if _, err := recoverAllocatedForTest(t, d, ctx, 0, 1); err != nil {
				t.Fatal(err)
			}
			cleanup, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil || cleanup.Operation != "delete" || !stopNotRequested(cleanup) || providerEffectsOf(fake) != before {
				t.Fatalf("missing separate cleanup grant: %+v %v", cleanup, err)
			}
			history, err := d.srv.store.GetEphemeralChildLifecycle(ctx, cleanup.UID, cleanup.Incarnation, cleanup.OperationID)
			if err != nil || history.Kind != "failure_cleanup" {
				t.Fatalf("wrong cleanup provenance: %+v %v", history, err)
			}
			// Существующий observer подтверждает уже остановленную CT без повторного Stop.
			cleanup = observeFailureDetection(t, d, cleanup)
			if cleanup.Stage != "delete" || !deleteNotRequested(cleanup) {
				t.Fatalf("owned stopped cleanup did not advance: %+v", cleanup)
			}
			fake.uncertainDelete = unknownDelete
			_, _ = recoverAllocatedForTest(t, d, ctx, 0, 1)
			submitted, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil || submitted.Stage != "delete" || submitted.Phase == "deleted" || fake.deletes != 1 {
				t.Fatalf("delete bypassed requested boundary: %+v %v", submitted, err)
			}
			_, _ = recoverAllocatedForTest(t, d, ctx, 0, 1)
			final, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil {
				t.Fatal(err)
			}
			if unknownDelete {
				if final != submitted || final.DeletedAt != nil || final.UPID != "" || deleteNotRequested(final) {
					t.Fatalf("unknown Delete released or replayed: %+v", final)
				}
			} else if final.Phase != "deleted" || final.Stage != "done" {
				t.Fatalf("confirmed absence not finalized: %+v", final)
			}
			if fake.deletes != 1 || fake.stops != before.stops || fake.starts != before.starts || fake.exists {
				t.Fatal("cleanup replayed effect or failed to delete")
			}
			work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
			if err != nil || work.Task != child {
				t.Fatal("cleanup invented child report")
			}
			if _, err := d.srv.store.GetEphemeralChildResultSeal(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("cleanup invented seal: %v", err)
			}
		})
	}
}

func TestObservedWorkerStopHolds(t *testing.T) {
	for _, scenario := range []string{"running-session-dead", "absent", "ownership", "permission", "unknown", "no-lineage", "unsettled-start", "no-upid", "revoked-owner", "pool-revoked", "wrong-incarnation", "wrong-owner", "wrong-actor"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			d, intent, actor, child, fake := observedWorkerFixture(t, true)
			fake.running = false
			switch scenario {
			case "running-session-dead":
				fake.running = true
			case "absent":
				fake.exists = false
			case "ownership":
				fake.inspectError = proxmox.ErrOwnership
			case "permission":
				fake.inspectError = proxmox.ErrPermission
			case "unknown":
				fake.inspectError = proxmox.ErrUncertain
			case "no-lineage":
				createSQL(t, d, `UPDATE managed_actors SET operation_id='unproven-operation' WHERE uid=?`, actor.UID)
			case "unsettled-start":
				createSQL(t, d, `UPDATE managed_actors SET stage='start' WHERE uid=?`, actor.UID)
			case "no-upid":
				createSQL(t, d, `UPDATE managed_actors SET upid='' WHERE uid=?`, actor.UID)
			case "revoked-owner":
				if err := d.srv.store.SetUserDisabled(ctx, d.master.DelegatorID, true); err != nil {
					t.Fatal(err)
				}
			case "pool-revoked":
				createSQL(t, d, `UPDATE managed_pools SET state='draining' WHERE uid=?`, actor.PoolUID)
			case "wrong-incarnation":
				fake.spec.Owner.Incarnation++
			case "wrong-owner":
				fake.spec.Owner.Nonce = "different-owner"
			case "wrong-actor":
				fake.spec.Owner.ActorUID = "different-actor"
			}
			snapshot, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			if err != nil {
				t.Fatal(err)
			}
			before := providerEffectsOf(fake)
			_, _ = recoverAllocatedForTest(t, d, ctx, 0, 1)
			if _, err := d.srv.store.GetEphemeralChildExecutorFailure(ctx, intent.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("uncertain or unauthorized state became failure: %v", err)
			}
			current, err := d.srv.store.GetManagedActor(ctx, actor.UID)
			work, workErr := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
			if err != nil || workErr != nil || current != snapshot || work.Task != child || providerEffectsOf(fake) != before {
				t.Fatal("held work changed")
			}
		})
	}
}
