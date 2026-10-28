package server

import (
	"errors"
	"testing"
	"time"

	"brood.dev/go/brood/app"
)

// FEATURE: The database name constraint preserves idempotent creation under races.
// SPEC: A concurrent winner owned by the caller is returned; another owner's
// winner is never claimed. The race occurs after the preliminary name lookup.
func TestConcurrentSwarmNameInsertPreservesIdempotence(t *testing.T) {
	for _, owner := range []string{"alice", "bob"} {
		t.Run(owner, func(t *testing.T) {
			g, _, prov := swarmRaceFixture(t)
			interleaved := &insertRaceStore{Store: g}
			interleaved.beforeSave = func() error {
				return g.SaveSwarm(app.Swarm{
					ID: "winner", ProjectID: "p", OwnerID: owner, Name: "race",
					AtespaceName: "swarm-winner", AtespaceUID: "winner-uid", State: app.StateActive,
				})
			}
			svc := app.NewService(interleaved, prov, time.Now, func() string { return "loser" })
			got, err := svc.CreateSwarm("alice", "p", "race")
			if owner == "alice" {
				if err != nil || got.ID != "winner" || got.State != app.StateActive {
					t.Fatalf("idempotent create = %+v, %v", got, err)
				}
			} else if !errors.Is(err, app.ErrExists) {
				t.Fatalf("foreign winning name = %v, want already exists", err)
			}
			if _, exists, err := g.SwarmByID("loser"); err != nil || exists {
				t.Fatalf("losing create left a row: %v, %v", exists, err)
			}
		})
	}
}

type insertRaceStore struct {
	app.Store
	beforeSave func() error
}

func (s *insertRaceStore) SaveSwarm(swarm app.Swarm) error {
	if s.beforeSave != nil {
		if err := s.beforeSave(); err != nil {
			return err
		}
		s.beforeSave = nil
	}
	return s.Store.SaveSwarm(swarm)
}
