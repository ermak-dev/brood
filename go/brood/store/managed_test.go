package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// FEATURE: Durable managed Proxmox inventory.
// SPEC: A fresh database creates the full managed schema; reservations survive
// unknown cleanup and stopped/error phases; CAS and partial uniqueness prevent reuse.
func managedFixture(t *testing.T, s *Store) ManagedPool {
	t.Helper()
	ctx := context.Background()
	seedUser(t, s, "u", "managed-user")
	for _, id := range []string{"one", "two"} {
		seedProject(t, s, id, id)
		if err := s.CreateSwarm(ctx, Swarm{ID: id, ProjectID: id, OwnerID: "u", Name: id, AtespaceName: id, AtespaceUID: ptr("native-" + id), State: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.CreateManagedPool(ctx, ManagedPool{UID: "pool", Name: "pve", Connection: "configured", Source: ManagedSourceSnapshot{ClusterIdentity: "cluster", Endpoint: "https://pve.invalid:8006", Node: "node", ResourcePool: "brood", Storage: "local", Network: "vmbr0"}, Labels: map[string]string{"provider": "proxmox"}, MaxActors: 2, Resources: ManagedResources{CPUMillicores: 2000, MemoryBytes: 2048, DiskBytes: 4096}, Template: ManagedTemplateSnapshot{Profile: "claude-proxmox", URI: "local:template.tar.zst", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, State: "enabled"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func managedRequest(uid, swarm string, ctid int) ManagedActor {
	return ManagedActor{UID: uid, Name: uid, SwarmID: swarm, AtespaceUID: "native-" + swarm, PoolUID: "pool", Resources: ManagedResources{CPUMillicores: 1000, MemoryBytes: 1024, DiskBytes: 2048}, CTID: ctid, OwnerNonce: "owner-" + uid, OperationID: "create-" + uid}
}
func TestManagedReservationLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p := managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("actor", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	if a.Revision != 1 || a.Incarnation != 1 || a.Phase != "provisioning" || a.Desired != "running" || a.Operation != "create" || a.Template != p.Template || a.ClusterIdentity != p.Source.ClusterIdentity || a.CreatedAt == "" || a.SourceFingerprint != p.SourceFingerprint {
		t.Fatalf("reservation: %+v", a)
	}
	if _, err := s.UpdateManagedActor(ctx, a, 0); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update: %v", err)
	}
	a.Phase = "error"
	a.Error = "cleanup unknown"
	a, err = s.UpdateManagedActor(ctx, a, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	p.State = "draining"
	p, err = s.UpdateManagedPool(ctx, p, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveManagedActor(ctx, managedRequest("blocked", "two", 102)); !errors.Is(err, ErrPoolUnavailable) {
		t.Fatalf("draining reserve: %v", err)
	}
	if _, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "stop-1", "stop"); err == nil {
		t.Fatal("unresolved cleanup allowed replacement intent")
	}
	a.Phase = "running"
	a, err = s.UpdateManagedActor(ctx, a, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	a = lifecycleConfirm(t, s, a, "running")
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "stop-1", "stop")
	if err != nil {
		t.Fatal(err)
	}
	if a.Desired != "stopped" || a.OperationID != "stop-1" {
		t.Fatalf("intent: %+v", a)
	}
	a.Phase = "stopped"
	a.UPID = "UPID:stop"
	a, err = s.UpdateManagedActor(ctx, a, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	a = lifecycleConfirm(t, s, a, "stopped")
	if _, err := s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "start-1", "start"); !errors.Is(err, ErrPoolUnavailable) {
		t.Fatalf("draining start: %v", err)
	}
	if err := s.DeleteManagedPool(ctx, p.UID, p.Revision); !errors.Is(err, ErrManagedChildren) {
		t.Fatalf("pool delete: %v", err)
	}
	children, err := s.HasManagedActorsBySwarm(ctx, "one")
	if err != nil || !children {
		t.Fatalf("children=%v err=%v", children, err)
	}
	a, err = s.BeginManagedActorOperation(ctx, a.UID, a.Revision, "delete-1", "delete")
	if err != nil {
		t.Fatal(err)
	}
	a.Phase = "deleted"
	if _, err := s.UpdateManagedActor(ctx, a, a.Revision); err == nil {
		t.Fatal("unconfirmed delete accepted")
	}
	a.Phase = "deleting"
	a = lifecycleConfirm(t, s, a, "stopped")
	a, err = s.ConfirmManagedActorDeleted(ctx, a.UID, a.Revision, a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if children, err = s.HasManagedActorsBySwarm(ctx, "one"); err != nil || children {
		t.Fatalf("deleted children=%v err=%v", children, err)
	}
	if err := s.DeleteManagedPool(ctx, p.UID, p.Revision); err != nil {
		t.Fatal(err)
	}
}
func TestManagedCapacityAndUniqueness(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	p := managedFixture(t, s)
	a, err := s.ReserveManagedActor(ctx, managedRequest("a", "one", 101))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ManagedActor){func(b *ManagedActor) { b.Name = "a" }, func(b *ManagedActor) { b.CTID = 101 }} {
		b := managedRequest("b", "one", 102)
		change(&b)
		if _, err := s.ReserveManagedActor(ctx, b); !IsUniqueViolation(err) {
			t.Fatalf("collision: %v", err)
		}
	}
	changed := p
	changed.Source.Node = "other"
	if _, err := s.UpdateManagedPool(ctx, changed, p.Revision); !errors.Is(err, ErrManagedBinding) {
		t.Fatalf("binding: %v", err)
	}
	a.Template.Profile = "other"
	if _, err := s.UpdateManagedActor(ctx, a, a.Revision); !errors.Is(err, ErrManagedBinding) {
		t.Fatalf("actor binding: %v", err)
	}
	a, err = s.GetManagedActor(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	a.Phase = "error"
	a, err = s.UpdateManagedActor(ctx, a, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.ReserveManagedActor(ctx, managedRequest("b", "two", 102))
	if err != nil {
		t.Fatal(err)
	}
	b.Phase = "running"
	b, err = s.UpdateManagedActor(ctx, b, b.Revision)
	if err != nil {
		t.Fatal(err)
	}
	b = lifecycleConfirm(t, s, b, "running")
	b, err = s.BeginManagedActorOperation(ctx, b.UID, b.Revision, "stop-b", "stop")
	if err != nil {
		t.Fatal(err)
	}
	b.Phase = "stopped"
	if _, err = s.UpdateManagedActor(ctx, b, b.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveManagedActor(ctx, managedRequest("c", "two", 103)); !errors.Is(err, ErrPoolCapacity) {
		t.Fatalf("error/stopped not reserved: %v", err)
	}
	actors, err := s.ListManagedActors(ctx, "")
	if err != nil || len(actors) != 2 {
		t.Fatalf("list: %v %v", actors, err)
	}
	actors, err = s.ListManagedActors(ctx, "one")
	if err != nil || len(actors) != 1 {
		t.Fatalf("swarm list: %v %v", actors, err)
	}
	pools, err := s.ListManagedPools(ctx)
	if err != nil || len(pools) != 1 {
		t.Fatalf("pools: %v %v", pools, err)
	}
}
func TestManagedConcurrentReservations(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	other, err := Open(ctx, func() string {
		var p string
		if err := s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			st := s
			if i%2 == 0 {
				st = other
			}
			_, err := st.ReserveManagedActor(ctx, managedRequest(fmt.Sprint(i), []string{"one", "two"}[i%2], 100+i))
			results <- err
		})
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrPoolCapacity) {
			t.Fatal(err)
		}
	}
	if accepted != 2 {
		t.Fatalf("accepted=%d want 2", accepted)
	}
}

// TestManagedSchemaContract: миграций нет (#205); база чужой версии
// отвергается без изменения схемы и данных, частичная схема с чужими
// managed-объектами — тоже.
func TestManagedSchemaContract(t *testing.T) {
	for _, version := range []int{0, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx := context.Background()
			path := legacyACLDatabase(t, false)
			if version == 2 {
				db, err := sql.Open("sqlite", dsn(path))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(swarmMembersSchema + `; INSERT INTO swarm_members SELECT id,owner_id,'owner' FROM swarms; ` + liveSwarmNameIndex + `; ` + projectCapacitySchema + `; PRAGMA user_version=2`); err != nil {
					db.Close()
					t.Fatal(err)
				}
				db.Close()
			}
			db, err := sql.Open("sqlite", dsn(path))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE managed_actors (foreign_data TEXT); INSERT INTO managed_actors VALUES ('keep')`); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, db)
			db.Close()
			if st, err := Open(ctx, path); err == nil {
				st.Close()
				t.Fatal("partial managed schema accepted")
			} else if !errors.Is(err, errIncompatibleSchema) {
				t.Fatal(err)
			}
			db, err = sql.Open("sqlite", dsn(path))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var v int
			if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
				t.Fatal(err)
			}
			if v != version || !reflect.DeepEqual(before, schemaSnapshot(t, db)) {
				t.Fatal("refused open mutated database")
			}
			var data string
			if err := db.QueryRow(`SELECT foreign_data FROM managed_actors`).Scan(&data); err != nil || data != "keep" {
				t.Fatalf("reserved data=%q err=%v", data, err)
			}
		})
	}
}

// FEATURE: Адресация актора по имени в рое для канала сообщений (#115).
// SPEC: LookupManagedActor(swarmID, name) находит живой (не deleted) актёр
// своего роя; чужой рой, чужое имя и tombstone — ErrNotFound.
func TestLookupManagedActorByName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	if _, err := s.ReserveManagedActor(ctx, func() ManagedActor {
		a := managedRequest("uid-a", "one", 101)
		a.Name = "claude"
		return a
	}()); err != nil {
		t.Fatal(err)
	}
	got, err := s.LookupManagedActor(ctx, "one", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != "uid-a" {
		t.Fatalf("lookup: %+v", got)
	}
	if _, err := s.LookupManagedActor(ctx, "two", "claude"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign swarm must not resolve: %v", err)
	}
	if _, err := s.LookupManagedActor(ctx, "one", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown name must not resolve: %v", err)
	}
}

// FEATURE: Каталог подключений Proxmox, создаётся из панели (#118).
// SPEC: подключение хранит публичные поля и write-only секрет токена;
// секрет не читается обратно; миграция v4->v5 сохраняет данные.
func TestManagedConnectionCatalog(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	c, err := s.CreateManagedConnection(ctx, ManagedConnection{Name: "pve-main", Endpoint: "https://pve.invalid:8006", TokenID: "op@pve!brood", TokenSecret: "secret-one", CAPEM: "-----BEGIN CERTIFICATE-----", Node: "n", ResourcePool: "r", Storage: "s", Network: "vmbr0"})
	if err != nil {
		t.Fatal(err)
	}
	if c.UID == "" || c.CreatedAt == "" {
		t.Fatalf("connection: %+v", c)
	}
	// Список не возвращает секрет.
	list, err := s.ListManagedConnections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].TokenSecret != "" {
		t.Fatalf("list leaked or lost the secret: %+v", list)
	}
	if _, err := s.CreateManagedConnection(ctx, ManagedConnection{Name: "pve-main", Endpoint: "https://x.invalid", TokenID: "a@b!c", TokenSecret: "s", Node: "n", ResourcePool: "r", Storage: "s", Network: "v"}); err == nil {
		t.Fatal("duplicate connection name must be refused")
	}
	if err := s.DeleteManagedConnection(ctx, c.UID); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.ListManagedConnections(ctx); len(list) != 0 {
		t.Fatalf("delete: %+v", list)
	}
}
