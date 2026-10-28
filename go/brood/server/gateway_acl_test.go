package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// FEATURE: Gateway persists ACL changes with authorization inside the write transaction.
// SPEC: An editor cannot escalate; stale actor grants, target activity or membership
// cannot authorize a write after the service's preliminary checks.
func TestGatewayACLAuthorizationIsRechecked(t *testing.T) {
	for _, scenario := range []string{"actor grant revoked", "target disabled", "target removed"} {
		t.Run(scenario, func(t *testing.T) {
			g, db, prov := swarmRaceFixture(t)
			if err := g.SetSwarmMember("admin", "s", "admin", "owner"); err != nil {
				t.Fatal(err)
			}
			interleaved := &interleavedACLStore{Store: g}
			interleaved.beforeGrant = func() {
				var err error
				switch scenario {
				case "actor grant revoked":
					err = db.SetSwarmMember(context.Background(), "s", "alice", "viewer")
				case "target disabled":
					err = db.SetUserDisabled(context.Background(), "bob", true)
				case "target removed":
					err = db.RemoveMember(context.Background(), "p", "bob")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			svc := app.NewService(interleaved, prov, time.Now, nil)
			if err := svc.SetSwarmMember("alice", "s", "bob", "editor"); !errors.Is(err, app.ErrNotAllowed) {
				t.Fatalf("stale grant = %v, want not allowed", err)
			}
			if _, err := db.GetSwarmRole(context.Background(), "s", "bob"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refused write left a grant: %v", err)
			}
		})
	}
}

func TestGatewayACLLastOwnerAndViewer(t *testing.T) {
	g, _, prov := swarmRaceFixture(t)
	svc := app.NewService(g, prov, time.Now, nil)
	if err := svc.SetSwarmMember("alice", "s", "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetSwarmMember("bob", "s", "bob", "owner"); !errors.Is(err, app.ErrNotAllowed) {
		t.Fatalf("viewer escalation = %v", err)
	}
	if err := svc.RemoveSwarmMember("alice", "s", "alice"); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("last owner removal = %v", err)
	}
	if allowed, err := svc.Can("bob", app.ActionViewSwarm, app.SwarmRes("s")); err != nil || !allowed {
		t.Fatalf("explicit viewer cannot read: %v, %v", allowed, err)
	}
}

type interleavedACLStore struct {
	app.Store
	beforeGrant func()
}

func (s *interleavedACLStore) SetSwarmMember(actorID, swarmID, userID, role string) error {
	if s.beforeGrant != nil {
		s.beforeGrant()
	}
	return s.Store.SetSwarmMember(actorID, swarmID, userID, role)
}
