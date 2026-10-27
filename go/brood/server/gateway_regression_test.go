package server

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// BUG: устаревшее удаление возвращает прежнего владельца после передачи роя.
// HYPOTHESIS: UpdateSwarm принимает владение из снимка вместо проверки в транзакции.
// SOLUTION: отдельная передача владения и проверка идентичности внутри WithTx.
// RESULT: FIXED — устаревшее удаление не вызывает провижинер и не меняет владельца.
// STATUS: FIXED
func TestGatewayTransferWinsBeforeDeletionClaim(t *testing.T) {
	g, db, prov := swarmRaceFixture(t)
	admin := app.NewService(g, prov, time.Now, nil)
	interleaved := &interleavedSwarmStore{Store: g}
	interleaved.beforeUpdate = func() {
		if err := admin.TransferSwarm("admin", "s", "bob"); err != nil {
			t.Fatalf("transfer: %v", err)
		}
	}
	alice := app.NewService(interleaved, prov, time.Now, nil)
	err := alice.DeleteSwarm("alice", "s")
	if !errors.Is(err, app.ErrConflict) {
		t.Errorf("stale deletion = %v, want conflict", err)
	}
	current, getErr := db.GetSwarm(context.Background(), "s")
	if getErr != nil || current.OwnerID != "bob" || current.State != "active" || current.Retry != "" {
		t.Errorf("transferred swarm overwritten: %+v, %v", current, getErr)
	}
	if prov.deletes != 0 || !prov.namespaceExists || !prov.actorExists {
		t.Errorf("stale deletion reached provisioner: calls=%d namespace=%v actor=%v", prov.deletes, prov.namespaceExists, prov.actorExists)
	}
}

func TestGatewayDeletionClaimWinsBeforeStaleTransfer(t *testing.T) {
	g, db, prov := swarmRaceFixture(t)
	alice := app.NewService(g, prov, time.Now, nil)
	interleaved := &interleavedSwarmStore{Store: g}
	interleaved.beforeBobRead = func() {
		if err := alice.DeleteSwarm("alice", "s"); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	admin := app.NewService(interleaved, prov, time.Now, nil)
	if err := admin.TransferSwarm("admin", "s", "bob"); !errors.Is(err, app.ErrState) {
		t.Errorf("stale transfer = %v, want state rejection", err)
	}
	current, err := db.GetSwarm(context.Background(), "s")
	if err != nil || current.OwnerID != "alice" || current.State != "deleted" || prov.deletes != 1 {
		t.Fatalf("deletion overwritten: %+v, calls=%d, %v", current, prov.deletes, err)
	}
}

func TestGatewayRejectsChangedLifecycleIdentity(t *testing.T) {
	for _, field := range []string{"owner", "project", "atespace"} {
		t.Run(field, func(t *testing.T) {
			g, db, _ := swarmRaceFixture(t)
			sw, _, err := g.SwarmByID("s")
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "owner":
				sw.OwnerID = "bob"
			case "project":
				sw.ProjectID = "another-project"
			case "atespace":
				sw.AtespaceName = "another-namespace"
			}
			sw.State, sw.Retry, sw.AtespaceUID = app.StateDeleting, "delete", "another-uid"
			if err := g.UpdateSwarm(sw); !errors.Is(err, app.ErrConflict) {
				t.Errorf("changed %s accepted: %v", field, err)
			}
			current, err := db.GetSwarm(context.Background(), "s")
			if err != nil || current.OwnerID != "alice" || current.State != "active" || current.Retry != "" || current.AtespaceUID == nil || *current.AtespaceUID != "uid" {
				t.Fatalf("rejected update changed row: %+v, %v", current, err)
			}
		})
	}
}

func TestGatewayTransferRejectedWhileDeleting(t *testing.T) {
	g, db, prov := swarmRaceFixture(t)
	interleaved := &interleavedSwarmStore{Store: g}
	interleaved.beforeBobRead = func() {
		sw, _, err := g.SwarmByID("s")
		if err != nil {
			t.Fatal(err)
		}
		sw.State, sw.Retry = app.StateDeleting, "delete"
		if err := g.UpdateSwarm(sw); err != nil {
			t.Fatal(err)
		}
	}
	admin := app.NewService(interleaved, prov, time.Now, nil)
	if err := admin.TransferSwarm("admin", "s", "bob"); !errors.Is(err, app.ErrState) {
		t.Errorf("transfer during deleting = %v, want state rejection", err)
	}
	current, err := db.GetSwarm(context.Background(), "s")
	if err != nil || current.OwnerID != "alice" || current.State != "deleting" || current.Retry != "delete" {
		t.Fatalf("deleting claim overwritten: %+v, %v", current, err)
	}
}

func TestGatewayRevokedOwnerCannotTransition(t *testing.T) {
	g, db, prov := swarmRaceFixture(t)
	stale, _, err := g.SwarmByID("s")
	if err != nil {
		t.Fatal(err)
	}
	admin := app.NewService(g, prov, time.Now, nil)
	if err := admin.TransferSwarm("admin", "s", "bob"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []app.SwarmState{app.StateDeleting, app.StateError, app.StateActive, app.StateDeleted} {
		stale.State = state
		if err := g.UpdateSwarm(stale); !errors.Is(err, app.ErrConflict) {
			t.Errorf("revoked owner transitioned to %s: %v", state, err)
		}
	}
	if err := g.ChangeSwarmOwner("s", "alice", "admin"); !errors.Is(err, app.ErrConflict) {
		t.Errorf("stale transfer = %v, want conflict", err)
	}
	current, err := db.GetSwarm(context.Background(), "s")
	if err != nil || current.OwnerID != "bob" || current.State != "active" {
		t.Fatalf("revoked owner changed row: %+v, %v", current, err)
	}
}

func TestGatewayTransferRechecksNewOwner(t *testing.T) {
	for _, revoke := range []string{"disabled", "membership", "missing"} {
		t.Run(revoke, func(t *testing.T) {
			g, db, prov := swarmRaceFixture(t)
			interleaved := &interleavedSwarmStore{Store: g}
			interleaved.beforeOwnerChange = func() {
				var err error
				switch revoke {
				case "disabled":
					err = db.SetUserDisabled(context.Background(), "bob", true)
				case "membership":
					err = db.RemoveMember(context.Background(), "p", "bob")
				case "missing":
					err = db.DeleteUser(context.Background(), "bob")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			admin := app.NewService(interleaved, prov, time.Now, nil)
			if err := admin.TransferSwarm("admin", "s", "bob"); !errors.Is(err, app.ErrNotAllowed) {
				t.Errorf("ineligible new owner accepted: %v", err)
			}
			current, err := db.GetSwarm(context.Background(), "s")
			if err != nil || current.OwnerID != "alice" || current.State != "active" {
				t.Fatalf("rejected transfer changed row: %+v, %v", current, err)
			}
		})
	}
}

type interleavedSwarmStore struct {
	app.Store
	beforeUpdate      func()
	beforeBobRead     func()
	beforeOwnerChange func()
}

func (s *interleavedSwarmStore) ChangeSwarmOwner(id, expectedOwnerID, newOwnerID string) error {
	if hook := s.beforeOwnerChange; hook != nil {
		s.beforeOwnerChange = nil
		hook()
	}
	return s.Store.ChangeSwarmOwner(id, expectedOwnerID, newOwnerID)
}

func (s *interleavedSwarmStore) UpdateSwarm(sw app.Swarm) error {
	if hook := s.beforeUpdate; hook != nil {
		s.beforeUpdate = nil
		hook()
	}
	return s.Store.UpdateSwarm(sw)
}

func (s *interleavedSwarmStore) UserByID(id string) (app.User, bool, error) {
	if hook := s.beforeBobRead; id == "bob" && hook != nil {
		s.beforeBobRead = nil
		hook()
	}
	return s.Store.UserByID(id)
}

type swarmRaceProvisioner struct {
	deletes         int
	namespaceExists bool
	actorExists     bool
}

func (p *swarmRaceProvisioner) GetAtespace(string) (string, error)    { return "uid", nil }
func (p *swarmRaceProvisioner) CreateAtespace(string) (string, error) { return "uid", nil }
func (p *swarmRaceProvisioner) DeleteAtespace(string, string) error {
	p.deletes++
	p.namespaceExists, p.actorExists = false, false
	return nil
}

func swarmRaceFixture(t *testing.T) (*Gateway, *store.Store, *swarmRaceProvisioner) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "brood.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, id := range []string{"admin", "alice", "bob"} {
		if err := db.CreateUser(ctx, store.User{ID: id, Username: id, PasswordHash: "unused", IsAdmin: id == "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateProject(ctx, store.Project{ID: "p", Name: "p"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob"} {
		if err := db.AddMember(ctx, "p", id); err != nil {
			t.Fatal(err)
		}
	}
	g := StoreGateway(db, nil)
	if err := g.SaveSwarm(app.Swarm{ID: "s", ProjectID: "p", OwnerID: "alice", Name: "s", AtespaceName: "swarm-s", AtespaceUID: "uid", State: app.StateActive}); err != nil {
		t.Fatal(err)
	}
	return g, db, &swarmRaceProvisioner{namespaceExists: true, actorExists: true}
}

func TestGatewayDoesNotRestoreStaleAdministratorFlag(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "brood.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []string{"a", "b"} {
		if err := db.CreateUser(ctx, store.User{ID: id, Username: id, PasswordHash: "unused", IsAdmin: true}); err != nil {
			t.Fatal(err)
		}
	}
	gateway := StoreGateway(db, nil)
	stale, _, err := gateway.UserByID("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserAdmin(ctx, "a", false); err != nil {
		t.Fatal(err)
	}
	stale.Disabled = true
	if err := gateway.UpdateUser(stale); err != nil {
		t.Fatal(err)
	}
	current, err := db.GetUser(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if current.IsAdmin || !current.Disabled {
		t.Fatalf("restored stale privilege: %+v", current)
	}
}

func TestGatewayPersistsSwarmRetryIntent(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "brood.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateUser(ctx, store.User{ID: "u", Username: "u", PasswordHash: "unused"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProject(ctx, store.Project{ID: "p", Name: "p"}); err != nil {
		t.Fatal(err)
	}
	gateway := StoreGateway(db, nil)
	swarm := app.Swarm{ID: "s", ProjectID: "p", OwnerID: "u", Name: "s", AtespaceName: "swarm-s", State: app.StateError, Retry: "delete"}
	if err := gateway.SaveSwarm(swarm); err != nil {
		t.Fatal(err)
	}
	got, ok, err := gateway.SwarmByID("s")
	if err != nil || !ok {
		t.Fatalf("read: %v", err)
	}
	if got.Retry != "delete" {
		t.Fatalf("lost retry intent: %+v", got)
	}
	got.Retry = "create"
	if err := gateway.UpdateSwarm(got); err != nil {
		t.Fatal(err)
	}
	got, _, err = gateway.SwarmByID("s")
	if err != nil || got.Retry != "create" {
		t.Fatalf("lost updated retry: %+v, %v", got, err)
	}
}
