package store

// BUG: Browser lifetime reservation не должна отменять явный lifecycle stop.
// HYPOTHESIS: Общий workspace guard не отличает активный browser stream от mutation RPC.
// SOLUTION: Явный stop текущего inflight stream меняет desired, сохраняя workspace slot.
// RESULT: FIXED — stop отзывает stream, но не разрешает child, unknown и старую инкарнацию.
// STATUS: FIXED

import (
	"context"
	"errors"
	"testing"
)

func TestBrowserReservationAllowsExplicitStopWithoutRelease(t *testing.T) {
	for _, operation := range []string{"terminal_stream", "slash", "session_terminal"} {
		for _, state := range []string{"inflight", "unconfirmed"} {
			t.Run(operation+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				s, p, e := engineFixture(t)
				op, err := s.AcquireActorOperation(ctx, e.ActorUID, e.Incarnation, NewID(), "browser:u", operation)
				mustNil(t, err)
				if state == "unconfirmed" {
					mustNil(t, s.FinishActorOperation(ctx, op, false))
				}
				a, err := s.GetManagedActor(ctx, e.ActorUID)
				mustNil(t, err)
				stopped, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, NewID(), "stop")
				if operation == "terminal_stream" && state == "inflight" {
					if err != nil || stopped.Desired != "stopped" {
						t.Fatalf("explicit stop refused: %+v %v", stopped, err)
					}
				} else if !errors.Is(err, ErrCoordinationConflict) {
					t.Fatalf("stop bypassed unresolved mutation: %v", err)
				}
				held, err := s.CurrentActorOperation(ctx, e.ActorUID)
				mustNil(t, err)
				if held.OperationID != op.OperationID || held.State != state {
					t.Fatalf("stop released or completed effect: %+v", held)
				}
				if _, err := s.EnqueueSwarmChild(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "after-stop", "test", "must not dispatch", childAllow); err == nil {
					t.Fatal("stop admitted child before stream reconciliation")
				}
			})
		}
	}
}

func TestBrowserOldIncarnationReservationStillHoldsStop(t *testing.T) {
	ctx := context.Background()
	s, _, e := engineFixture(t)
	_, err := s.AcquireActorOperation(ctx, e.ActorUID, e.Incarnation, NewID(), "browser:u", "terminal_stream")
	mustNil(t, err)
	setActor(t, s, e.ActorUID, "incarnation=2")
	a, err := s.GetManagedActor(ctx, e.ActorUID)
	mustNil(t, err)
	if _, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, NewID(), "stop"); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("old incarnation hold bypassed: %v", err)
	}
}

func TestSessionReservationChildIndependentConnectionRace(t *testing.T) {
	for range 12 {
		ctx := context.Background()
		s, p, e := engineFixture(t)
		var seq int
		var name, path string
		mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path))
		other, err := Open(ctx, path)
		mustNil(t, err)
		func() {
			defer other.Close()
			start := make(chan struct{})
			mutated := make(chan error, 1)
			enqueued := make(chan error, 1)
			go func() {
				<-start
				_, err := s.AcquireActorOperation(ctx, e.ActorUID, e.Incarnation, NewID(), "operator:u", "slash")
				mutated <- err
			}()
			go func() {
				<-start
				_, err := other.EnqueueSwarmChild(ctx, p, SwarmChildJoinPolicy{Mode: "all", Total: 2, Remaining: "retain"}, "racing", "test", "must serialize", childAllow)
				enqueued <- err
			}()
			close(start)
			mutationErr, childErr := <-mutated, <-enqueued
			if (mutationErr == nil) == (childErr == nil) {
				t.Fatalf("expected exactly one winner: mutation=%v child=%v", mutationErr, childErr)
			}
			if mutationErr != nil && !errors.Is(mutationErr, ErrCoordinationConflict) {
				t.Fatal(mutationErr)
			}
			if childErr != nil && !errors.Is(childErr, ErrCoordinationConflict) {
				t.Fatal(childErr)
			}
		}()
	}
}
