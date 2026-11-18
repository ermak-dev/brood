package store

// BUG: Child scheduling holds deny explicit owner session control.
// HYPOTHESIS: Automatic allocation exclusion is incorrectly used as owner authority.
// SOLUTION: Journal authorized owner effects separately and retain interrupted attempts.

import (
	"context"
	"errors"
	"testing"
)

func TestOwnerLifecycleInterventionAtomicFailure(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	reserveChild(t, s, p, e, "queued")
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	before := dumpTables(t, s, "actor_interventions", "actor_step_interruptions", "swarm_steps", "managed_actors")
	mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
		a.Revision++
		if _, err := s.BeginOwnerManagedActorOperation(ctx, a, "bad-revision", "stop", sw.OwnerID, func(context.Context) error { return nil }); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("wrong revision accepted: %v", err)
		}
		return nil
	}))
	if got := dumpTables(t, s, "actor_interventions", "actor_step_interruptions", "swarm_steps", "managed_actors"); got != before {
		t.Fatal("handled owner refusal leaked intervention inside outer transaction")
	}
}

func TestOwnerInterventionLateCompletionRetainsUncertainty(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	c := reserveChild(t, s, p, e, "running")
	c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
	mustNil(t, err)
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	op, err := s.AcquireOwnerActorOperation(ctx, a, "interrupt", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil })
	mustNil(t, err)
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	// The executor's observed report is preserved, but cannot automatically bless
	// the interrupted parent execution or release the child's automatic hold.
	c, err = s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "late evidence", childAllow)
	mustNil(t, err)
	j, err := s.JoinSwarmChildren(ctx, p, childAllow)
	mustNil(t, err)
	if !j.Interrupted || j.Evaluation.State != "unconfirmed" || c.Report != "late evidence" {
		t.Fatalf("late completion erased uncertainty: %+v", j)
	}
	step, err := s.GetSwarmStep(ctx, p.Principal.SwarmID, p.Sequence)
	mustNil(t, err)
	if step.State != "unconfirmed" {
		t.Fatalf("late success advanced parent: %+v", step)
	}
	if err := s.CheckActorChildObligations(ctx, a.UID); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("interrupted allocation automatically released: %v", err)
	}
	if _, err := s.AcquireOwnerActorOperation(ctx, a, "interrupt", "human", "session_terminal", sw.OwnerID, func(context.Context) error { return nil }); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("owner effect replay granted a second send: %v", err)
	}
}

func TestOwnerInterventionDoesNotGrantForeignCaller(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	reserveChild(t, s, p, e, "queued")
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	before := dumpTables(t, s, "actor_interventions", "actor_step_interruptions", "swarm_steps")
	for _, human := range []string{"missing", "ed", "vw"} {
		if _, err := s.AcquireOwnerActorOperation(ctx, a, "foreign-"+human, "foreign", "session_terminal", human, func(context.Context) error { return nil }); !errors.Is(err, ErrSwarmChildDenied) {
			t.Fatalf("foreign intervention accepted for %s: %v", human, err)
		}
	}
	if got := dumpTables(t, s, "actor_interventions", "actor_step_interruptions", "swarm_steps"); got != before {
		t.Fatal("foreign refusal changed execution")
	}
}

func TestOwnerCanStopAndResetRunningChild(t *testing.T) {
	for _, operation := range []string{"stop", "reset"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			s, p, e := childFixture(t, "all", 1, 0)
			c := reserveChild(t, s, p, e, "running")
			c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
			mustNil(t, err)
			a, err := s.GetManagedActor(ctx, e.ActorUID)
			mustNil(t, err)
			sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
			mustNil(t, err)
			next, err := s.BeginOwnerManagedActorOperation(ctx, a, "owner-"+operation, operation, sw.OwnerID, func(context.Context) error { return nil })
			mustNil(t, err)
			if next.Phase != "stopping" {
				t.Fatalf("owner lifecycle not recorded: %+v", next)
			}
			if _, err := s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "stale completion", childAllow); !errors.Is(err, ErrSwarmChildDenied) {
				t.Fatalf("stopping incarnation completion accepted: %v", err)
			}
			w, err := s.SwarmChildWorkByID(ctx, c.ID)
			mustNil(t, err)
			if w.Task.State != "running" {
				t.Fatalf("running task orphaned by owner lifecycle: %+v", w)
			}
		})
	}
}

func TestOwnerSessionInterventionWithRunningChild(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 2, 0)
	c := reserveChild(t, s, p, e, "running")
	c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
	mustNil(t, err)
	target, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	sw, err := s.GetSwarm(ctx, p.Principal.SwarmID)
	mustNil(t, err)
	op, err := s.AcquireActorSessionOperation(ctx, target, "owner-input", "operator:"+sw.OwnerID, "session_terminal", CoordinationActor{}, sw.OwnerID, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("explicit owner input denied by automatic child hold: %v", err)
	}
	mustNil(t, s.FinishActorOperation(ctx, op, true))
	// The effect cannot turn the running child into success or erase its record.
	j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if len(j.Children) != 1 || j.Children[0].State != "running" {
		t.Fatalf("running history changed: %+v", j)
	}
	step, err := s.GetSwarmStep(ctx, p.Principal.SwarmID, p.Sequence)
	mustNil(t, err)
	if step.State != "unconfirmed" {
		t.Fatalf("parent still claims uninterrupted execution: %s", step.State)
	}
	if _, err := s.AcquireActorOperation(ctx, target.UID, target.Incarnation, "automatic-restart", "automatic", "restart"); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("automatic conflicting mutation allowed: %v", err)
	}
}
