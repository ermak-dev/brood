package store

// FEATURE: Настройка проекта и immutable network snapshot актора (#302).
// SPEC: Новые акторы получают только свою project policy; изменение/очистка
// default не переписывает старые snapshots; session/ACL проверяются в транзакции.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/egress"
)

func networkFixture(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	p := managedFixture(t, s)
	p.MaxActors = 8
	p.Resources = ManagedResources{CPUMillicores: 8000, MemoryBytes: 8192, DiskBytes: 16384}
	_, err := s.UpdateManagedPool(ctx, p, p.Revision)
	mustNil(t, err)
	seedUser(t, s, "network-admin", "network-admin")
	mustNil(t, s.SetUserAdmin(ctx, "network-admin", true))
	seedUser(t, s, "backup-admin", "backup-admin")
	mustNil(t, s.SetUserAdmin(ctx, "backup-admin", true))
	mustNil(t, s.AddMember(ctx, "one", "u"))
	return s, ctx
}
func networkWire(t *testing.T, host string) string {
	t.Helper()
	p, err := egress.Compile([]egress.Rule{{Protocol: egress.TLS, Hostnames: []string{host}, Ports: []int{8443}}})
	mustNil(t, err)
	raw, err := p.Encode()
	mustNil(t, err)
	return string(raw)
}
func syncNetwork(t *testing.T, s *Store, ctx context.Context, wire *string) ProjectSyncResult {
	t.Helper()
	result, err := s.SyncProject(ctx, "network-admin", "one", ProjectDeclaration{Name: "one", ManagedNetwork: wire}, func(context.Context) bool { return true })
	mustNil(t, err)
	return result
}

func TestManagedNetworkPinsEachActorAndSurvivesDefaultChanges(t *testing.T) {
	s, ctx := networkFixture(t)
	first := networkWire(t, "first.example")
	second := networkWire(t, "second.example")
	result := syncNetwork(t, s, ctx, &first)
	if !result.Changed {
		t.Fatal("new network not recorded")
	}
	a, err := s.ReserveManagedActor(ctx, managedRequest("first", "one", 101))
	mustNil(t, err)
	foreign, err := s.ReserveManagedActor(ctx, managedRequest("foreign", "two", 102))
	mustNil(t, err)
	if a.NetworkPolicy != first || foreign.NetworkPolicy != "" {
		t.Fatal("snapshot lost project isolation")
	}
	result = syncNetwork(t, s, ctx, nil)
	if result.Changed {
		t.Fatal("omission changed selected network")
	}
	got, err := s.ProjectManagedNetwork(ctx, "one")
	mustNil(t, err)
	if got != first {
		t.Fatal("omission cleared network")
	}
	syncNetwork(t, s, ctx, &second)
	b, err := s.ReserveManagedActor(ctx, managedRequest("second", "one", 103))
	mustNil(t, err)
	if b.NetworkPolicy != second {
		t.Fatal("new actor missed new default")
	}
	empty := ""
	syncNetwork(t, s, ctx, &empty)
	c, err := s.ReserveManagedActor(ctx, managedRequest("legacy", "one", 104))
	mustNil(t, err)
	if c.NetworkPolicy != "" {
		t.Fatal("explicit clear did not restore legacy for a new actor")
	}
	want := map[string]string{"first": first, "second": second, "foreign": "", "legacy": ""}
	actors, err := s.ListManagedActors(ctx, "")
	mustNil(t, err)
	if len(actors) != len(want) {
		t.Fatalf("actor count=%d", len(actors))
	}
	for _, actor := range actors {
		if actor.NetworkPolicy != want[actor.UID] {
			t.Fatalf("roster rewrote %s policy", actor.UID)
		}
		stored, err := s.GetManagedActor(ctx, actor.UID)
		mustNil(t, err)
		if stored.NetworkPolicy != want[actor.UID] {
			t.Fatalf("read rewrote %s policy", actor.UID)
		}
	}
}

func TestManagedNetworkCannotBeSuppliedOrChangedByActorUpdates(t *testing.T) {
	s, ctx := networkFixture(t)
	wire := networkWire(t, "project.example")
	syncNetwork(t, s, ctx, &wire)
	request := managedRequest("actor", "one", 101)
	request.NetworkPolicy = networkWire(t, "caller.example")
	a, err := s.ReserveManagedActor(ctx, request)
	mustNil(t, err)
	if a.NetworkPolicy != wire {
		t.Fatal("caller overrode authoritative project policy")
	}
	before := a
	a.NetworkPolicy = networkWire(t, "changed.example")
	if _, err := s.UpdateManagedActor(ctx, a, a.Revision); !errors.Is(err, ErrManagedBinding) {
		t.Fatalf("network update: %v", err)
	}
	stored, err := s.GetManagedActor(ctx, a.UID)
	mustNil(t, err)
	if !reflect.DeepEqual(stored, before) {
		t.Fatal("refused update changed actor")
	}
	for _, query := range []string{
		`UPDATE managed_actor_networks SET policy='{}' WHERE actor_uid='actor'`,
		`DELETE FROM managed_actor_networks WHERE actor_uid='actor'`,
		`INSERT OR REPLACE INTO managed_actor_networks(actor_uid,policy) VALUES ('actor','{}')`,
	} {
		if _, err := s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "managed actor network snapshot is immutable") {
			t.Fatalf("snapshot fence: %v", err)
		}
	}
}

func TestManagedNetworkHistorySurvivesActorIdentityRemoval(t *testing.T) {
	s, ctx := networkFixture(t)
	wire := networkWire(t, "retained.example")
	syncNetwork(t, s, ctx, &wire)
	a, err := s.ReserveManagedActor(ctx, managedRequest("actor", "one", 101))
	mustNil(t, err)
	_, err = s.db.Exec(`DELETE FROM managed_actors WHERE uid=?`, a.UID)
	mustNil(t, err)
	var retained string
	mustNil(t, s.db.QueryRow(`SELECT policy FROM managed_actor_networks WHERE actor_uid=?`, a.UID).Scan(&retained))
	if retained != wire {
		t.Fatal("identity removal changed or erased network history")
	}
	if _, err := s.ReserveManagedActor(ctx, managedRequest("actor", "one", 102)); err == nil {
		t.Fatal("old actor UID reused despite immutable snapshot history")
	}
}

func TestManagedNetworkSyncChecksCurrentAuthorityAndCanonicalPolicy(t *testing.T) {
	s, ctx := networkFixture(t)
	wire := networkWire(t, "tool.example")
	authorize := func(context.Context) bool { return true }
	declaration := ProjectDeclaration{Name: "one", ManagedNetwork: &wire}
	if _, err := s.SyncProject(ctx, "u", "one", declaration, authorize); !errors.Is(err, ErrProjectSyncDenied) {
		t.Fatalf("member changed network: %v", err)
	}
	if _, err := s.SyncProject(ctx, "network-admin", "one", declaration, func(context.Context) bool { return false }); !errors.Is(err, ErrProjectSyncDenied) {
		t.Fatalf("invalid session changed network: %v", err)
	}
	bad := `{"version":2,"rules":[]}`
	declaration.ManagedNetwork = &bad
	if _, err := s.SyncProject(ctx, "network-admin", "one", declaration, authorize); err == nil {
		t.Fatal("invalid policy recorded")
	}
	stored, err := s.ProjectManagedNetwork(ctx, "one")
	mustNil(t, err)
	if stored != "" {
		t.Fatal("denial wrote default")
	}
	syncNetwork(t, s, ctx, &wire)
	declaration.ManagedNetwork = &wire
	if result, err := s.SyncProject(ctx, "u", "one", declaration, authorize); err != nil || result.Changed {
		t.Fatalf("idempotent member sync: %+v %v", result, err)
	}
	mustNil(t, s.SetUserDisabled(ctx, "network-admin", true))
	empty := ""
	declaration.ManagedNetwork = &empty
	if _, err := s.SyncProject(ctx, "network-admin", "one", declaration, authorize); !errors.Is(err, ErrProjectSyncDenied) {
		t.Fatalf("disabled editor cleared network: %v", err)
	}
}
