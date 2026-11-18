package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// BUG: Управление allocated executor до регистрации не прерывало parent step.
// HYPOTHESIS: Поиск затронутых родителей учитывает только swarm_child_tasks.
// SOLUTION: Immutable allocation связывает executor с точным parent request;
// общий postcondition требует сохранённую interruption и unconfirmed step.
// RESULT: FIXED — ready/provisioning, ignored writes и outer rollback проверены.
// STATUS: FIXED
func TestDelegatedInterventionBeforeRegistrationRequiresRetainedInterruption(t *testing.T) {
	ctx := context.Background()
	for _, ignored := range []string{"", "actor_step_interruptions", "swarm_steps"} {
		t.Run(ignored, func(t *testing.T) {
			s, job := ephemeralAttestationFixture(t, true)
			swarm, err := s.GetSwarm(ctx, job.Parent.Principal.SwarmID)
			mustNil(t, err)
			mustNil(t, s.SetUserAdmin(ctx, swarm.OwnerID, true))
			_, err = s.db.Exec(`UPDATE managed_actors SET role='master',delegator_id=? WHERE uid=?`, swarm.OwnerID, job.Parent.Principal.ActorUID)
			mustNil(t, err)
			source, err := s.GetManagedActor(ctx, job.Parent.Principal.ActorUID)
			mustNil(t, err)
			target, err := s.GetManagedActor(ctx, "isolated")
			mustNil(t, err)
			if ignored != "" {
				event := "INSERT"
				if ignored == "swarm_steps" {
					event = "UPDATE"
				}
				_, err = s.db.Exec(fmt.Sprintf("CREATE TRIGGER ignore_pre_registration BEFORE %s ON %s BEGIN SELECT RAISE(IGNORE); END", event, ignored))
				mustNil(t, err)
			}
			before := databaseRows(t, s.db)
			mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
				_, err := s.BeginDelegatedManagedActorOperation(ctx, source, target, "delegated-before-registration", "stop", swarm.OwnerID, func(context.Context) error { return nil })
				if ignored == "" {
					return err
				}
				if err == nil {
					t.Fatal("delegated control accepted without retained parent interruption")
				}
				return nil
			}))
			if ignored != "" {
				if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
					t.Fatal("ignored interruption leaked explicit lifecycle intent")
				}
			} else {
				step, err := s.GetSwarmStep(ctx, swarm.ID, job.Parent.Sequence)
				mustNil(t, err)
				if step.State != "unconfirmed" {
					t.Fatalf("delegated control left allocated parent active: %+v", step)
				}
			}
		})
	}
}

func TestOwnerInterventionBeforeRegistrationRejectsIgnoredInterruption(t *testing.T) {
	ctx := context.Background()
	for _, table := range []string{"actor_step_interruptions", "swarm_steps"} {
		t.Run(table, func(t *testing.T) {
			s, job := ephemeralAttestationFixture(t, false)
			a, err := s.GetManagedActor(ctx, "isolated")
			mustNil(t, err)
			swarm, err := s.GetSwarm(ctx, a.SwarmID)
			mustNil(t, err)
			event := "INSERT"
			if table == "swarm_steps" {
				event = "UPDATE"
			}
			_, err = s.db.Exec(fmt.Sprintf("CREATE TRIGGER ignore_owner_parent BEFORE %s ON %s BEGIN SELECT RAISE(IGNORE); END", event, table))
			mustNil(t, err)
			before := databaseRows(t, s.db)
			mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
				if _, err := s.AcquireOwnerActorOperation(ctx, a, "owner-ignored", "operator:"+swarm.OwnerID, "stop", swarm.OwnerID, func(context.Context) error { return nil }); err == nil {
					t.Fatalf("owner control accepted without retained parent interruption for job %s", job.ID)
				}
				return nil
			}))
			if !reflect.DeepEqual(before, databaseRows(t, s.db)) {
				t.Fatal("ignored owner interruption leaked retained authority")
			}
		})
	}
}

func TestOwnerInterventionInterruptsUnregisteredEphemeralParent(t *testing.T) {
	ctx := context.Background()
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "provisioning", true: "ready"}[ready], func(t *testing.T) {
			s, job := ephemeralAttestationFixture(t, ready)
			allocation, err := s.GetEphemeralChildAllocation(ctx, job.ID)
			mustNil(t, err)
			actor, err := s.GetManagedActor(ctx, allocation.ActorUID)
			mustNil(t, err)
			swarm, err := s.GetSwarm(ctx, actor.SwarmID)
			mustNil(t, err)
			before := databaseRows(t, s.db)
			op, err := s.AcquireOwnerActorOperation(ctx, actor, "owner-before-registration", "operator:"+swarm.OwnerID, "stop", swarm.OwnerID, func(context.Context) error { return nil })
			mustNil(t, err)
			mustNil(t, s.FinishActorOperation(ctx, op, true))
			step, err := s.GetSwarmStep(ctx, actor.SwarmID, job.Parent.Sequence)
			mustNil(t, err)
			if step.State != "unconfirmed" {
				t.Fatalf("pre-registration intervention left parent step active: %+v", step)
			}
			join, err := s.GetSwarmChildJoin(ctx, job.Parent, childAllow)
			mustNil(t, err)
			if !join.Interrupted || join.Evaluation.State != "unconfirmed" || len(join.Children) != 0 {
				t.Fatalf("intervention did not retain the actual interrupted logical group: %+v", join)
			}
			after := databaseRows(t, s.db)
			for _, table := range []string{"ephemeral_child_intents", "ephemeral_child_allocations", "ephemeral_child_create_grants", "swarm_child_tasks"} {
				if !reflect.DeepEqual(before[table], after[table]) {
					t.Fatalf("intervention rewrote original facts in %s", table)
				}
			}
		})
	}
}
