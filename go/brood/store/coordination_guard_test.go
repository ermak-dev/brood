package store

import (
	"context"
	"errors"
	"strings"
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
		for _, op := range []string{"stop", "delete", "reset", "update"} {
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

// BUG: A lifecycle refusal caused by an active claim named neither its holder
// nor its request ID, so the reader could not tell whom to ask or what to
// release (#272).
// SPEC: The refusal names the holder and the dispatch target as actors of the
// guarded actor's swarm, the request ID and the release command. A holder that
// is not that live incarnation of the swarm is unknown, not guessed. A claim of
// another swarm is not described at all.

// guardRefusal берёт аренду c (или landing, если у c нет цели) и возвращает
// отказ stop актора uid.
func guardRefusal(t *testing.T, s *Store, c CoordinationClaim, uid string, change string) error {
	t.Helper()
	ctx := context.Background()
	if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET phase='running',operation='start',stage='done'`); err != nil {
		t.Fatal(err)
	}
	take := s.ClaimDispatchLease
	if c.Target == (CoordinationActor{}) {
		take = s.TakeLandingToken
	}
	if _, err := take(ctx, c); err != nil {
		t.Fatal(err)
	}
	if change != "" {
		if _, err := s.conn(ctx).ExecContext(ctx, change); err != nil {
			t.Fatal(err)
		}
	}
	actor, err := s.GetManagedActor(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.BeginManagedActorOperation(ctx, uid, actor.Revision, NewID(), "stop")
	if !errors.Is(err, ErrCoordinationConflict) {
		t.Fatalf("stop of %s under a claim: %v", uid, err)
	}
	return err
}

func TestCoordinationGuardNamesHolder(t *testing.T) {
	want := `dispatch lease "task-272" on "target" is held by "source"; the holder or an administrator releases it with dispatch --action release --request-id "task-272"`
	for _, uid := range []string{"source", "target"} {
		t.Run(uid, func(t *testing.T) {
			s := newTestStore(t)
			c := coordinationFixture(t, s)
			c.RequestID = "task-272"
			if err := guardRefusal(t, s, c, uid, ""); !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal: %v\nwant it to say: %s", err, want)
			}
		})
	}
	t.Run("landing", func(t *testing.T) {
		s := newTestStore(t)
		c := coordinationFixture(t, s)
		c.RequestID, c.Target = "land-272", CoordinationActor{}
		want := `landing token "land-272" is held by "source"; the holder or an administrator gives it back with landing --action give --request-id "land-272"`
		if err := guardRefusal(t, s, c, "source", ""); !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal: %v\nwant it to say: %s", err, want)
		}
	})
}

func TestCoordinationGuardHolderUnknown(t *testing.T) {
	for name, change := range map[string]string{
		"stale generation": `UPDATE managed_actors SET incarnation=2 WHERE uid='source'`,
		"deleted":          `UPDATE managed_actors SET phase='deleted' WHERE uid='source'`,
		"another swarm":    `UPDATE managed_actors SET swarm_id='two',atespace_uid='native-two' WHERE uid='source'`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			c := coordinationFixture(t, s)
			c.RequestID = "task-272"
			// Незнакомый держатель свою аренду не отпустит: точный release
			// остаётся администратору.
			want := `dispatch lease "task-272" on "target" is held by an unknown actor; an administrator releases it with dispatch --action release --request-id "task-272"`
			if err := guardRefusal(t, s, c, "target", change); !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), `"source"`) {
				t.Fatalf("refusal: %v\nwant it to say: %s", err, want)
			}
		})
	}
}

func TestCoordinationGuardHidesAnotherSwarm(t *testing.T) {
	s := newTestStore(t)
	c := coordinationFixture(t, s)
	c.RequestID = "task-272"
	err := guardRefusal(t, s, c, "target", `UPDATE managed_actors SET swarm_id='two',atespace_uid='native-two' WHERE uid='target'`)
	if !strings.Contains(err.Error(), "an active coordination claim outside this swarm") {
		t.Fatalf("refusal: %v", err)
	}
	for _, private := range []string{"task-272", `"source"`} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("refusal leaks %s of another swarm: %v", private, err)
		}
	}
}

func TestCoordinationGuardCountsOtherClaims(t *testing.T) {
	s := newTestStore(t)
	c := coordinationFixture(t, s)
	land := c
	land.RequestID, land.Target = "land-272", CoordinationActor{}
	if _, err := s.TakeLandingToken(context.Background(), land); err != nil {
		t.Fatal(err)
	}
	if err := guardRefusal(t, s, c, "source", ""); !strings.Contains(err.Error(), "(and 1 more)") {
		t.Fatalf("refusal hides the second claim: %v", err)
	}
}
