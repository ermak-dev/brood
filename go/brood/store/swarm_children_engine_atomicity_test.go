package store

// FEATURE: Atomic child-group creation inside an existing transaction (#289).
// SPEC: A refused first child must not leave an empty join if a trusted caller
// handles the refusal and commits its outer transaction.
// IMPLEMENTATION: A local savepoint protects the compound join/child reservation.
// RESULT: IMPLEMENTED — full store suite and focused race checks pass.
// STATUS: DONE

import (
	"context"
	"errors"
	"testing"
)

func TestChildEngineNestedEnqueueRefusalIsAtomic(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{"invalid payload", "reserve policy"} {
		t.Run(reason, func(t *testing.T) {
			s := dispatchStore(t)
			attempt := attestOne(t, s, "parent")
			mustNil(t, s.FinishActorOperation(ctx, attempt.Lease, true))
			_, err := s.db.Exec(`INSERT INTO swarm_role_bindings(swarm_id,role,actor_uid,actor_incarnation) VALUES('one','test','b',1)`)
			mustNil(t, err)
			p := SwarmChildParent{Principal: SwarmChildPrincipal{ProjectID: "one", SwarmID: "one", AtespaceUID: "native-one", ActorUID: "a", Incarnation: 1}, Pass: 1, Sequence: 1, RequestID: "parent"}
			payload := "Task"
			authorize := SwarmChildAuthorize(childAllow)
			if reason == "invalid payload" {
				payload = ""
			} else {
				authorize = func(_ context.Context, a SwarmChildAuthorization) error {
					if a.Action == "reserve" {
						return ErrSwarmChildDenied
					}
					return nil
				}
			}
			mustNil(t, s.WithTx(ctx, func(ctx context.Context) error {
				_, err := s.EnqueueSwarmChild(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 1, Remaining: "retain"}, "one", "test", payload, authorize)
				if !errors.Is(err, ErrSwarmChildConflict) && !errors.Is(err, ErrSwarmChildDenied) {
					t.Fatalf("missing refusal: %v", err)
				}
				return nil
			}))
			var groups, tasks int
			mustNil(t, s.db.QueryRow(`SELECT count(*) FROM swarm_child_joins`).Scan(&groups))
			mustNil(t, s.db.QueryRow(`SELECT count(*) FROM swarm_child_tasks`).Scan(&tasks))
			if groups != 0 || tasks != 0 {
				t.Fatalf("refused enqueue leaked durable barriers: joins=%d children=%d", groups, tasks)
			}
		})
	}
}
