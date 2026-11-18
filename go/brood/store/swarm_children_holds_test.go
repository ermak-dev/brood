package store

// FEATURE: Child-aware allocation and lifecycle holds (#289).
// SPEC: Pending/running children and incomplete parent groups protect actor
// lifecycle and workspace mutations. Running child allocations exclude another
// child and coordination. Cancellation releases only demonstrably unsent intent.
// IMPLEMENTATION: Shared transactional child-aware lifecycle and allocation guards.
// RESULT: IMPLEMENTED — store suite and focused race checks pass.
// STATUS: DONE

import (
	"context"
	"errors"
	"testing"
)

func TestSwarmChildLifecycleHolds(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"incomplete", "pending", "running"} {
		for _, uid := range []string{"a", "b"} {
			for _, operation := range []string{"stop", "delete", "reset", "workspace"} {
				t.Run(state+"/"+uid+"/"+operation, func(t *testing.T) {
					s, p, e := childFixture(t, "all", 2, 0)
					if state != "incomplete" {
						c := reserveChild(t, s, p, e, "held")
						if state == "running" {
							_, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
							mustNil(t, err)
						}
					} else if uid == "b" {
						return
					}
					a, err := s.GetManagedActor(ctx, uid)
					mustNil(t, err)
					before := dumpTables(t, s, "managed_actors", "swarm_child_tasks", "swarm_child_joins", "coordination_claims")
					if operation == "workspace" {
						_, err = s.AcquireActorOperation(ctx, uid, 1, "mutation", "human", "restart")
					} else {
						_, err = s.BeginManagedActorOperation(ctx, uid, a.Revision, "mutation", operation)
					}
					if !errors.Is(err, ErrCoordinationConflict) {
						t.Fatalf("%s accepted with %s children: %v", operation, state, err)
					}
					if after := dumpTables(t, s, "managed_actors", "swarm_child_tasks", "swarm_child_joins", "coordination_claims"); after != before {
						t.Fatal("refusal changed durable state")
					}
				})
			}
		}
	}
}

func TestSwarmChildExclusiveAllocation(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 2, 0)
	first := reserveChild(t, s, p, e, "first")
	second := reserveChild(t, s, p, e, "second")
	first, err := s.StartSwarmChild(ctx, p, first.RequestID, e, first.Revision, childAllow)
	mustNil(t, err)
	if _, err := s.StartSwarmChild(ctx, p, second.RequestID, e, second.Revision, childAllow); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("same worker executes two child tasks: %v", err)
	}
	_, err = s.CompleteSwarmChild(ctx, p, first.RequestID, e, first.Revision, "success", "first report", childAllow)
	mustNil(t, err)
	_, err = s.StartSwarmChild(ctx, p, second.RequestID, e, second.Revision, childAllow)
	mustNil(t, err)
}

func TestSwarmChildStartRespectsExistingCoordination(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 1, 0)
	c := reserveChild(t, s, p, e, "child")
	// A lease acquired before scheduling must not be silently stolen.
	_, err := s.db.Exec(`INSERT INTO coordination_claims(project_id,kind,request_id,payload,slot) VALUES('one','dispatch','existing','{}','b')`)
	mustNil(t, err)
	if _, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("child stole existing allocation: %v", err)
	}
}
