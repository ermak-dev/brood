package store

import (
	"context"
	"errors"
	"testing"
)

// FEATURE: Coordination claims guard conflicting lifecycle operations.
// SPEC: Both lease source and target remain protected until exact explicit release.
func TestCoordinationRefusesStoppedIntent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c := coordinationFixture(t, s)
	if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET desired='stopped' WHERE uid='target'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimDispatchLease(ctx, c); !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("stopped target acquired lease: %v", err)
	}
}

func TestCoordinationGuardsLifecycleIntent(t *testing.T) {
	for _, uid := range []string{"source", "target"} {
		for _, op := range []string{"stop", "delete", "reset"} {
			t.Run(uid+"-"+op, func(t *testing.T) {
				s := newTestStore(t)
				ctx := context.Background()
				c := coordinationFixture(t, s)
				if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET phase='running',operation='start',stage='done'`); err != nil {
					t.Fatal(err)
				}
				if _, err := s.ClaimDispatchLease(ctx, c); err != nil {
					t.Fatal(err)
				}
				actor, err := s.GetManagedActor(ctx, uid)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.BeginManagedActorOperation(ctx, uid, actor.Revision, NewID(), op); !errors.Is(err, ErrCoordinationConflict) {
					t.Fatalf("%s %s did not respect lease: %v", uid, op, err)
				}
				after, err := s.GetManagedActor(ctx, uid)
				if err != nil {
					t.Fatal(err)
				}
				if after != actor {
					t.Fatal("refused lifecycle changed actor")
				}
				if err := s.ReleaseDispatchLease(ctx, c); err != nil {
					t.Fatal(err)
				}
				if _, err := s.BeginManagedActorOperation(ctx, uid, actor.Revision, NewID(), op); err != nil {
					t.Fatalf("released actor stayed blocked: %v", err)
				}
			})
		}
	}
}
