package store

// BUG: Явное управление своим worker не отличалось от automatic lifecycle.
// HYPOTHESIS: Для actor caller не было транзакционного положительного origin.
// SOLUTION: Делегация и ownership проверяются вместе с retained intervention.
// RESULT: FIXED — actor attribution, reset lineage и atomic refusal проверены.
// STATUS: FIXED

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func delegatedInterventionFixture(t *testing.T) (*Store, SwarmChildParent, SwarmChildTask, ManagedActor, ManagedActor) {
	t.Helper()
	ctx := context.Background()
	s, p, executor := childFixture(t, "all", 1, 0)
	c := reserveChild(t, s, p, executor, "running")
	c, err := s.StartSwarmChild(ctx, p, c.RequestID, executor, c.Revision, childAllow)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	mustNil(t, s.SetUserAdmin(ctx, sw.OwnerID, true))
	_, err = s.db.Exec(`UPDATE managed_actors SET role='master',delegator_id=? WHERE uid=?`, sw.OwnerID, p.Principal.ActorUID)
	mustNil(t, err)
	_, err = s.db.Exec(`UPDATE managed_actors SET parent_uid=? WHERE uid=?`, p.Principal.ActorUID, executor.ActorUID)
	mustNil(t, err)
	source, err := s.GetManagedActor(ctx, p.Principal.ActorUID)
	mustNil(t, err)
	target, err := s.GetManagedActor(ctx, executor.ActorUID)
	mustNil(t, err)
	return s, p, c, source, target
}

func TestDelegatedLifecycleInterruptsRunningChild(t *testing.T) {
	for _, kind := range []string{"start", "stop", "reset", "delete", "update"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, p, child, source, target := delegatedInterventionFixture(t)
			before, err := s.GetOwnerManagedActorOperation(ctx, target.UID, target.Incarnation, target.OperationID)
			if !errors.Is(err, ErrNotFound) || before != (ActorOperation{}) {
				t.Fatalf("missing journal inferred permission: %+v %v", before, err)
			}
			next, err := s.BeginDelegatedManagedActorOperation(ctx, source, target, "delegated-"+kind, kind, source.DelegatorID, func(context.Context) error { return nil })
			mustNil(t, err)
			origin, err := s.GetOwnerManagedActorOperation(ctx, target.UID, target.Incarnation, next.OperationID)
			mustNil(t, err)
			if origin.Caller != fmt.Sprintf("actor:%s:%d", source.UID, source.Incarnation) || origin.Operation != kind || origin.State != "completed" {
				t.Fatalf("untruthful origin: %+v", origin)
			}
			var anchor string
			mustNil(t, s.db.QueryRow(`SELECT human_id FROM actor_interventions WHERE id=?`, next.OperationID).Scan(&anchor))
			if anchor != source.DelegatorID {
				t.Fatalf("lost delegation anchor: %q", anchor)
			}
			work, err := s.SwarmChildWorkByID(ctx, child.ID)
			mustNil(t, err)
			if work.Task != child {
				t.Fatalf("intervention rewrote child facts: %+v", work.Task)
			}
			step, err := s.GetSwarmStep(ctx, p.Principal.SwarmID, p.Sequence)
			mustNil(t, err)
			if step.State != "unconfirmed" || !strings.Contains(step.Detail, "delegated actor") || strings.Contains(step.Detail, "owner intervention") {
				t.Fatalf("misattributed interruption: %+v", step)
			}
			if err := s.CheckActorChildObligations(ctx, target.UID); !errors.Is(err, ErrCoordinationConflict) {
				t.Fatalf("automatic child hold cleared: %v", err)
			}
		})
	}
}

func TestDelegatedLifecycleRejectsCurrentAuthorityChanges(t *testing.T) {
	for _, scenario := range []string{"nil_callback", "callback_refused", "stale_source", "source_scope", "source_namespace", "source_not_ready", "source_not_master", "changed_delegator", "revoked_admin", "disabled_admin", "wrong_human", "operator_worker", "other_master_worker", "peer_master", "self", "target_scope", "target_namespace", "swarm_namespace", "inactive_swarm", "stale_target", "stale_revision", "unsupported", "adopt_automatic_id", "callback_revokes_admin", "callback_changes_namespace", "callback_reparents_target"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, parent, child, source, target := delegatedInterventionFixture(t)
			human, kind, id := source.DelegatorID, "stop", "denied"
			authorize := func(context.Context) error { return nil }
			switch scenario {
			case "nil_callback":
				authorize = nil
			case "callback_refused":
				authorize = func(context.Context) error { return ErrSwarmChildDenied }
			case "callback_revokes_admin":
				authorize = func(tx context.Context) error { return s.SetUserAdmin(tx, human, false) }
			case "callback_changes_namespace":
				authorize = func(tx context.Context) error {
					_, err := s.conn(tx).ExecContext(tx, `UPDATE swarms SET atespace_uid='changed' WHERE id=?`, source.SwarmID)
					return err
				}
			case "callback_reparents_target":
				authorize = func(tx context.Context) error {
					_, err := s.conn(tx).ExecContext(tx, `UPDATE managed_actors SET parent_uid='other' WHERE uid=?`, target.UID)
					return err
				}
			case "stale_source":
				source.Incarnation++
			case "source_scope":
				source.SwarmID = "other"
			case "source_namespace":
				source.AtespaceUID = "other"
			case "source_not_ready":
				setActor(t, s, source.UID, "stage='stop'")
			case "source_not_master":
				setActor(t, s, source.UID, "role='worker'")
			case "changed_delegator":
				source.DelegatorID = "other"
			case "revoked_admin":
				mustNil(t, s.SetUserAdmin(ctx, human, false))
			case "disabled_admin":
				mustNil(t, s.SetUserDisabled(ctx, human, true))
			case "wrong_human":
				human = "ed"
			case "operator_worker":
				setActor(t, s, target.UID, "parent_uid=''")
			case "other_master_worker":
				setActor(t, s, target.UID, "parent_uid='other'")
			case "peer_master":
				setActor(t, s, target.UID, "role='master'")
			case "self":
				target = source
			case "target_scope":
				target.SwarmID = "other"
			case "target_namespace":
				target.AtespaceUID = "other"
			case "swarm_namespace":
				_, err := s.db.Exec(`UPDATE swarms SET atespace_uid='other' WHERE id=?`, source.SwarmID)
				mustNil(t, err)
			case "inactive_swarm":
				_, err := s.CompleteSwarmChild(ctx, parent, child.RequestID, child.Executor, child.Revision, "success", "observed before swarm deletion", childAllow)
				mustNil(t, err)
				_, err = s.db.Exec(`UPDATE swarms SET state='deleted' WHERE id=?`, source.SwarmID)
				mustNil(t, err)
			case "stale_target":
				target.Incarnation++
			case "stale_revision":
				target.Revision++
			case "unsupported":
				kind = "restart"
			case "adopt_automatic_id":
				kind, id = target.Operation, target.OperationID
			}
			before := databaseRows(t, s.db)
			mustNil(t, s.WithTx(ctx, func(tx context.Context) error {
				if _, err := s.BeginDelegatedManagedActorOperation(tx, source, target, id, kind, human, authorize); err == nil {
					t.Fatal("invalid delegated command accepted")
				}
				return nil
			}))
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("handled refusal leaked writes into outer transaction")
			}
		})
	}
}

func TestDelegatedLifecycleResetRetainsExactDerivedOrigin(t *testing.T) {
	ctx := context.Background()
	s, _, child, source, target := delegatedInterventionFixture(t)
	a, err := s.BeginDelegatedManagedActorOperation(ctx, source, target, "delegated-reset", "reset", source.DelegatorID, func(context.Context) error { return nil })
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "absent")
	a, err = s.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "new-delegated-nonce")
	mustNil(t, err)
	a = lifecycleConfirm(t, s, a, "stopped")
	if _, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "automatic-start", "start"); err == nil {
		t.Fatal("delegated reset released automatic holds")
	}
	a, err = s.ContinueOwnerManagedActorReset(ctx, a, "delegated-first-start", func(context.Context, OwnerManagedActorLineage) error { return nil })
	mustNil(t, err)
	link, err := s.GetDerivedOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID)
	mustNil(t, err)
	if link.Origin.OperationID != "delegated-reset" || link.Origin.Caller != fmt.Sprintf("actor:%s:%d", source.UID, source.Incarnation) || link.Origin.Generation != target.Incarnation || a.Incarnation != target.Incarnation+1 {
		t.Fatalf("derived origin misattributed: %+v", link)
	}
	if _, err := s.GetOwnerManagedActorOperation(ctx, a.UID, a.Incarnation, a.OperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("continuation fabricated direct manual origin: %v", err)
	}
	work, err := s.SwarmChildWorkByID(ctx, child.ID)
	mustNil(t, err)
	if work.Task != child {
		t.Fatal("reset continuation rewrote original child evidence")
	}
}

func TestDelegatedLifecyclePendingBoundStepAtomicity(t *testing.T) {
	for _, ignored := range []string{"", "actor_step_interruptions", "swarm_steps"} {
		t.Run("ignored_"+ignored, func(t *testing.T) {
			ctx := context.Background()
			s := dispatchStore(t)
			setActor(t, s, "b", "role='master', delegator_id='u'")
			setActor(t, s, "a", "parent_uid='b'")
			mustNil(t, s.SetUserAdmin(ctx, "u", true))
			source, err := s.GetManagedActor(ctx, "b")
			mustNil(t, err)
			target, err := s.GetManagedActor(ctx, "a")
			mustNil(t, err)
			if step := stepOf(t, s, 1); step.State != "pending" || step.ActorUID != "" {
				t.Fatalf("fixture not pending before reservation: %+v", step)
			}
			if ignored != "" {
				event := "INSERT"
				if ignored == "swarm_steps" {
					event = "UPDATE"
				}
				_, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER ignored_pending BEFORE %s ON %s BEGIN SELECT RAISE(IGNORE); END", event, ignored))
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			err = s.WithTx(ctx, func(tx context.Context) error {
				_, err := s.BeginDelegatedManagedActorOperation(tx, source, target, "delegated-pending", "stop", "u", func(context.Context) error { return nil })
				if ignored != "" {
					if err == nil {
						t.Fatal("pending interruption was silently ignored")
					}
					return nil
				}
				return err
			})
			mustNil(t, err)
			if ignored != "" {
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("ignored pending interruption leaked writes")
				}
			} else if step := stepOf(t, s, 1); step.State != "unconfirmed" {
				t.Fatalf("pending binding escaped interruption: %+v", step)
			}
		})
	}
}

func TestDelegatedLifecycleIgnoredWorkspaceWriteAtomic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	target := ownerHistoryActor(t, s)
	_, err := s.AcquireActorOperation(ctx, target.UID, target.Incarnation, "unknown-workspace", "automatic", "restart")
	mustNil(t, err)
	source, err := s.ReserveManagedActor(ctx, managedRequest("master", "one", 202))
	mustNil(t, err)
	setActor(t, s, source.UID, "role='master', delegator_id='u', phase='running', stage='done', operation='start'")
	setActor(t, s, target.UID, "parent_uid='master'")
	mustNil(t, s.SetUserAdmin(ctx, "u", true))
	source, err = s.GetManagedActor(ctx, source.UID)
	mustNil(t, err)
	target, err = s.GetManagedActor(ctx, target.UID)
	mustNil(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER ignored_workspace BEFORE UPDATE ON coordination_claims BEGIN SELECT RAISE(IGNORE); END`)
	mustNil(t, err)
	before := databaseRows(t, s.db)
	mustNil(t, s.WithTx(ctx, func(tx context.Context) error {
		if _, err := s.BeginDelegatedManagedActorOperation(tx, source, target, "no-workspace-write", "stop", "u", func(context.Context) error { return nil }); err == nil {
			t.Fatal("ignored workspace interruption accepted")
		}
		return nil
	}))
	if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
		t.Fatal("ignored workspace update leaked intervention")
	}
}

func TestDelegatedLifecycleAtomicFailedOrIgnoredWrites(t *testing.T) {
	for _, effect := range []string{"ABORT", "IGNORE"} {
		for _, table := range []string{"actor_interventions", "actor_interventions_settle", "actor_step_interruptions", "swarm_steps", "managed_actors"} {
			t.Run(effect+"_"+table, func(t *testing.T) {
				ctx := context.Background()
				s, _, _, source, target := delegatedInterventionFixture(t)
				event := "INSERT"
				if table == "swarm_steps" || table == "managed_actors" {
					event = "UPDATE"
				}
				if table == "actor_interventions_settle" {
					table, event = "actor_interventions", "UPDATE"
				}
				raise := "RAISE(IGNORE)"
				if effect == "ABORT" {
					raise = "RAISE(ABORT, 'injected failure')"
				}
				_, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER delegated_write BEFORE %s ON %s BEGIN SELECT %s; END", event, table, raise))
				mustNil(t, err)
				before := databaseRows(t, s.db)
				mustNil(t, s.WithTx(ctx, func(tx context.Context) error {
					if _, err := s.BeginDelegatedManagedActorOperation(tx, source, target, "failed-write", "stop", source.DelegatorID, func(context.Context) error { return nil }); err == nil {
						t.Fatal("missing durable write accepted")
					}
					return nil
				}))
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("failed write leaked partial intervention")
				}
			})
		}
	}
}
