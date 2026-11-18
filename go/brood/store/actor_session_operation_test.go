package store

// BUG: Session mutation не занимает allocation и допускает concurrent child enqueue.
// HYPOTHESIS: Workspace reservation не поддерживает session mutation и очередь его не сверяет.
// SOLUTION: Один durable workspace slot исключает mutation и enqueue в write transaction.
// RESULT: FIXED — unknown остаётся held до явного подтверждения, child не стартует.
// STATUS: FIXED

import (
	"context"
	"errors"
	"testing"
)

func TestSessionOperationExcludesChildEnqueue(t *testing.T) {
	for _, operation := range []string{"slash", "session_terminal", "terminal_stream"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			s, p, e := engineFixture(t)
			op, err := s.AcquireActorOperation(ctx, e.ActorUID, e.Incarnation, NewID(), "operator:u", operation)
			if err != nil {
				t.Fatalf("session reservation unavailable: %v", err)
			}
			if _, err := s.EnqueueSwarmChild(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "blocked", "test", "Must not run", childAllow); !errors.Is(err, ErrCoordinationConflict) {
				t.Fatalf("session reservation admitted child: %v", err)
			}
			j, err := s.GetSwarmChildJoin(ctx, p, childAllow)
			mustNil(t, err)
			if len(j.Children) != 0 {
				t.Fatal("refused enqueue leaked child")
			}
			mustNil(t, s.FinishActorOperation(ctx, op, false))
			mustNil(t, s.RecoverActorOperations(ctx))
			held, err := s.CurrentActorOperation(ctx, e.ActorUID)
			mustNil(t, err)
			if held.State != "unconfirmed" {
				t.Fatalf("lost unknown hold: %+v", held)
			}
			if _, err := s.EnqueueSwarmChild(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "unknown", "test", "Must not run", childAllow); !errors.Is(err, ErrCoordinationConflict) {
				t.Fatalf("unknown mutation admitted child: %v", err)
			}
			mustNil(t, s.FinishActorOperation(ctx, held, true))
			c := engineChild(t, s, p, "after-recovery")
			if _, err := s.AcquireActorOperation(ctx, e.ActorUID, e.Incarnation, NewID(), "operator:u", operation); !errors.Is(err, ErrCoordinationConflict) {
				t.Fatalf("existing child allowed mutation: %v", err)
			}
			_, err = s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
			mustNil(t, err)
		})
	}
}
