package store

// FEATURE: Ёмкость пула и привязки ролей перед запуском роя (#221).
// SPEC: RolesFit по графу и снимку роя отвечает, может ли рой занять все роли
// графа: каждая роль привязана, актор привязки running и должен работать,
// принадлежит рою в записанной инкарнации, его пул enabled, а различных
// акторов в пуле графу нужно не больше max_actors. Один актор занимает
// несколько ролей; отказ называет первую роль и пул.
// IMPLEMENTATION: Таблица случаев над чистой функцией и проверки метода
// хранилища, читающего снимок в транзакции вызывающего.
// RESULT: IMPLEMENTED — каждый случай отказа из #221 и #211 типизирован и
// называет роль и пул; метод ничего не пишет.
// STATUS: DONE

import (
	"context"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/pipeline"
)

// fitDoc — граф с actor-узлом на каждую роль; RolesFit читает только роли.
func fitDoc(roles ...string) *pipeline.Document {
	nodes := map[string]pipeline.Node{"done": {Type: "exit"}}
	for _, role := range roles {
		nodes["do-"+role] = pipeline.Node{Type: "actor", Role: role}
	}
	return &pipeline.Document{Name: "fit", Start: "done", Exit: "done", Nodes: nodes}
}

// fitActor — живой актор роя «one» в пуле pool.
func fitActor(uid, pool string) ManagedActor {
	return ManagedActor{UID: uid, SwarmID: "one", AtespaceUID: "native-one", PoolUID: pool, Incarnation: 1, Phase: "running", Desired: "running"}
}

// fitSnapshot — активный рой «one», пул «pve» на два актора, акторы a и b
// и привязки ролей review → a, build → b, test → a.
func fitSnapshot() SwarmRoleSnapshot {
	return SwarmRoleSnapshot{
		Swarm: Swarm{ID: "one", State: "active", AtespaceUID: ptr("native-one")},
		Bindings: []SwarmRoleBinding{
			{SwarmID: "one", Role: "build", ActorUID: "b", ActorIncarnation: 1},
			{SwarmID: "one", Role: "review", ActorUID: "a", ActorIncarnation: 1},
			{SwarmID: "one", Role: "test", ActorUID: "a", ActorIncarnation: 1},
		},
		Actors: map[string]ManagedActor{"a": fitActor("a", "pool"), "b": fitActor("b", "pool")},
		Pools:  map[string]ManagedPool{"pool": {UID: "pool", Name: "pve", MaxActors: 2, State: "enabled"}},
	}
}

// assertFitError — отказ типизирован и называет роль и пул.
func assertFitError(t *testing.T, err error, want error, role, pool string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("RolesFit: %v, want %v", err, want)
	}
	var fit *RoleFitError
	if !errors.As(err, &fit) {
		t.Fatalf("RolesFit refusal is not a RoleFitError: %v", err)
	}
	if fit.Role != role || fit.Pool != pool {
		t.Fatalf("refusal names role %q, pool %q; want role %q, pool %q (%v)", fit.Role, fit.Pool, role, pool, err)
	}
}

// TestRolesFitTwoActorsThreeRoles — три роли на двух акторах в пуле на два
// актора помещаются: один актор занимает несколько ролей явной привязкой.
func TestRolesFitTwoActorsThreeRoles(t *testing.T) {
	if err := RolesFit(fitDoc("build", "review", "test"), fitSnapshot()); err != nil {
		t.Fatalf("three roles on two actors: %v", err)
	}
}

// TestRolesFitOneActorAllRoles — все роли на одном акторе в пуле на одного.
func TestRolesFitOneActorAllRoles(t *testing.T) {
	snap := fitSnapshot()
	snap.Bindings[0].ActorUID = "a"
	snap.Pools["pool"] = ManagedPool{UID: "pool", Name: "pve", MaxActors: 1, State: "enabled"}
	if err := RolesFit(fitDoc("build", "review", "test"), snap); err != nil {
		t.Fatalf("all roles on one actor: %v", err)
	}
}

// TestRolesFitGraphWithoutActors — граф без actor-узлов ролей не требует.
func TestRolesFitGraphWithoutActors(t *testing.T) {
	snap := fitSnapshot()
	snap.Bindings = nil
	if err := RolesFit(fitDoc(), snap); err != nil {
		t.Fatalf("graph without roles: %v", err)
	}
}

// TestRolesFitIgnoresBindingsOutsideGraph — привязка роли, которой нет в
// графе, не участвует: её неживой актор не мешает запуску.
func TestRolesFitIgnoresBindingsOutsideGraph(t *testing.T) {
	snap := fitSnapshot()
	snap.Bindings = append(snap.Bindings, SwarmRoleBinding{SwarmID: "one", Role: "docs", ActorUID: "gone", ActorIncarnation: 1})
	gone := fitActor("gone", "pool")
	gone.Phase = "deleted"
	snap.Actors["gone"] = gone
	if err := RolesFit(fitDoc("build", "review"), snap); err != nil {
		t.Fatalf("binding outside the graph refused the start: %v", err)
	}
}

// TestRolesFitRefusals — каждый отказ из #221 и #211 типизирован и называет
// роль, а для проблем пула и пул.
func TestRolesFitRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*SwarmRoleSnapshot)
		want   error
		role   string
		pool   string
	}{
		{"unbound role", func(s *SwarmRoleSnapshot) { s.Bindings = append(s.Bindings[:1], s.Bindings[2:]...) }, ErrSwarmRoleUnbound, "review", ""},
		{"binding of another swarm", func(s *SwarmRoleSnapshot) { s.Bindings[1].SwarmID = "two" }, ErrSwarmRoleUnbound, "review", ""},
		{"bound actor missing", func(s *SwarmRoleSnapshot) { delete(s.Actors, "b") }, ErrSwarmRoleUnbound, "build", ""},
		{"actor provisioning", func(s *SwarmRoleSnapshot) { setFitActor(s, "b", func(a *ManagedActor) { a.Phase = "provisioning" }) }, ErrSwarmRoleInactive, "build", ""},
		{"actor stopped", func(s *SwarmRoleSnapshot) { setFitActor(s, "b", func(a *ManagedActor) { a.Phase = "stopped" }) }, ErrSwarmRoleInactive, "build", ""},
		{"actor error", func(s *SwarmRoleSnapshot) { setFitActor(s, "b", func(a *ManagedActor) { a.Phase = "error" }) }, ErrSwarmRoleInactive, "build", ""},
		{"actor deleting", func(s *SwarmRoleSnapshot) { setFitActor(s, "b", func(a *ManagedActor) { a.Phase = "deleting" }) }, ErrSwarmRoleInactive, "build", ""},
		{"actor deleted", func(s *SwarmRoleSnapshot) { setFitActor(s, "b", func(a *ManagedActor) { a.Phase = "deleted" }) }, ErrSwarmRoleInactive, "build", ""},
		{"actor desired stopped", func(s *SwarmRoleSnapshot) { setFitActor(s, "b", func(a *ManagedActor) { a.Desired = "stopped" }) }, ErrSwarmRoleInactive, "build", ""},
		{"actor desired deleted", func(s *SwarmRoleSnapshot) { setFitActor(s, "b", func(a *ManagedActor) { a.Desired = "deleted" }) }, ErrSwarmRoleInactive, "build", ""},
		{"actor of another swarm", func(s *SwarmRoleSnapshot) { setFitActor(s, "b", func(a *ManagedActor) { a.SwarmID = "two" }) }, ErrSwarmRoleForeignActor, "build", ""},
		{"actor in a stale atespace", func(s *SwarmRoleSnapshot) { setFitActor(s, "b", func(a *ManagedActor) { a.AtespaceUID = "stale" }) }, ErrSwarmRoleForeignActor, "build", ""},
		{"stale incarnation", func(s *SwarmRoleSnapshot) { setFitActor(s, "b", func(a *ManagedActor) { a.Incarnation = 2 }) }, ErrSwarmRoleStale, "build", ""},
		{"swarm not active", func(s *SwarmRoleSnapshot) { s.Swarm.State = "deleting" }, ErrSwarmRoleInactive, "build", ""},
		{"pool draining", func(s *SwarmRoleSnapshot) { setFitPool(s, func(p *ManagedPool) { p.State = "draining" }) }, ErrPoolUnavailable, "build", "pve"},
		{"pool deleted", func(s *SwarmRoleSnapshot) { setFitPool(s, func(p *ManagedPool) { p.State = "deleted" }) }, ErrPoolUnavailable, "build", "pve"},
		{"pool missing", func(s *SwarmRoleSnapshot) { delete(s.Pools, "pool") }, ErrPoolUnavailable, "build", "pool"},
		{"two actors, pool for one", func(s *SwarmRoleSnapshot) { setFitPool(s, func(p *ManagedPool) { p.MaxActors = 1 }) }, ErrPoolCapacity, "review", "pve"},
		{"three actors, pool for two", func(s *SwarmRoleSnapshot) {
			s.Bindings[2].ActorUID = "c"
			s.Actors["c"] = fitActor("c", "pool")
		}, ErrPoolCapacity, "test", "pve"},
		{"pool without slots", func(s *SwarmRoleSnapshot) { setFitPool(s, func(p *ManagedPool) { p.MaxActors = 0 }) }, ErrPoolCapacity, "build", "pve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := fitSnapshot()
			tc.change(&snap)
			assertFitError(t, RolesFit(fitDoc("build", "review", "test"), snap), tc.want, tc.role, tc.pool)
		})
	}
}

// setFitActor меняет актора снимка по UID.
func setFitActor(s *SwarmRoleSnapshot, uid string, change func(*ManagedActor)) {
	a := s.Actors[uid]
	change(&a)
	s.Actors[uid] = a
}

// setFitPool меняет пул «pool» снимка.
func setFitPool(s *SwarmRoleSnapshot, change func(*ManagedPool)) {
	p := s.Pools["pool"]
	change(&p)
	s.Pools["pool"] = p
}

// TestRolesFitCapacityIsPerPool — max_actors ограничивает акторов своего
// пула, а не всего роя: по актору в двух пулах на одного помещаются.
func TestRolesFitCapacityIsPerPool(t *testing.T) {
	snap := fitSnapshot()
	snap.Actors["b"] = fitActor("b", "spare")
	snap.Pools["pool"] = ManagedPool{UID: "pool", Name: "pve", MaxActors: 1, State: "enabled"}
	snap.Pools["spare"] = ManagedPool{UID: "spare", Name: "spare", MaxActors: 1, State: "enabled"}
	if err := RolesFit(fitDoc("build", "review", "test"), snap); err != nil {
		t.Fatalf("one actor per pool: %v", err)
	}
	snap.Pools["spare"] = ManagedPool{UID: "spare", Name: "spare", MaxActors: 1, State: "draining"}
	assertFitError(t, RolesFit(fitDoc("build", "review", "test"), snap), ErrPoolUnavailable, "build", "spare")
}

// TestRolesFitNamesFirstRole — при нескольких проблемах отказ называет
// первую роль по имени, независимо от порядка обхода карт.
func TestRolesFitNamesFirstRole(t *testing.T) {
	snap := fitSnapshot()
	snap.Bindings = snap.Bindings[:1]
	setFitPool(&snap, func(p *ManagedPool) { p.State = "draining" })
	for range 32 {
		assertFitError(t, RolesFit(fitDoc("test", "review", "build"), snap), ErrPoolUnavailable, "build", "pve")
	}
	snap = fitSnapshot()
	snap.Bindings = snap.Bindings[1:]
	for range 32 {
		assertFitError(t, RolesFit(fitDoc("test", "review", "build"), snap), ErrSwarmRoleUnbound, "build", "")
	}
}

// TestRolesFitMessageNamesRoleAndPool — причина отказа читается строкой:
// роль, пул и состояние, без трассировки.
func TestRolesFitMessageNamesRoleAndPool(t *testing.T) {
	snap := fitSnapshot()
	setFitPool(&snap, func(p *ManagedPool) { p.State = "draining" })
	err := RolesFit(fitDoc("build"), snap)
	for _, want := range []string{`role "build"`, `pool "pve"`, "draining"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %s", err, want)
		}
	}
	snap = fitSnapshot()
	setFitPool(&snap, func(p *ManagedPool) { p.MaxActors = 1 })
	err = RolesFit(fitDoc("build", "review"), snap)
	for _, want := range []string{`role "review"`, `pool "pve"`, "2 distinct actors", "max_actors 1"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("capacity refusal %q does not name %s", err, want)
		}
	}
}

// TestRolesFitRequiresDocument — без графа проверять нечего: отказ, а не успех.
func TestRolesFitRequiresDocument(t *testing.T) {
	if err := RolesFit(nil, fitSnapshot()); err == nil {
		t.Fatal("nil document fits")
	}
}

// fitStore — рой «one» с двумя running-акторами a и b в пуле «pool» на два
// актора и привязками build → b, review → a, test → a.
func fitStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t)
	managedFixture(t, s)
	for i, uid := range []string{"a", "b"} {
		if _, err := s.ReserveManagedActor(ctx, managedRequest(uid, "one", 201+i)); err != nil {
			t.Fatal(err)
		}
		setActor(t, s, uid, "phase='running'")
	}
	for role, uid := range map[string]string{"build": "b", "review": "a", "test": "a"} {
		if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: role, ActorUID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// TestCheckSwarmRolesFitStore — метод хранилища читает рой, привязки,
// акторов и пулы из базы и отказывает теми же ошибками, что чистая функция.
func TestCheckSwarmRolesFitStore(t *testing.T) {
	ctx := context.Background()
	doc := fitDoc("build", "review", "test")
	t.Run("fits", func(t *testing.T) {
		if err := fitStore(t).CheckSwarmRolesFit(ctx, "one", doc); err != nil {
			t.Fatalf("live bindings: %v", err)
		}
	})
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *Store)
		want  error
		role  string
		pool  string
	}{
		{"unbound role", func(t *testing.T, s *Store) { execFit(t, s, `DELETE FROM swarm_role_bindings WHERE role='review'`) }, ErrSwarmRoleUnbound, "review", ""},
		{"actor provisioning", func(t *testing.T, s *Store) { setActor(t, s, "b", "phase='provisioning'") }, ErrSwarmRoleInactive, "build", ""},
		{"actor stopped", func(t *testing.T, s *Store) { setActor(t, s, "b", "phase='stopped', desired='stopped'") }, ErrSwarmRoleInactive, "build", ""},
		{"actor of another swarm", func(t *testing.T, s *Store) { setActor(t, s, "b", "swarm_id='two', atespace_uid='native-two'") }, ErrSwarmRoleForeignActor, "build", ""},
		{"stale incarnation", func(t *testing.T, s *Store) { setActor(t, s, "b", "incarnation=incarnation+1") }, ErrSwarmRoleStale, "build", ""},
		{"pool draining", func(t *testing.T, s *Store) {
			p, err := s.GetManagedPool(ctx, "pool")
			if err != nil {
				t.Fatal(err)
			}
			p.State = "draining"
			if _, err := s.UpdateManagedPool(ctx, p, p.Revision); err != nil {
				t.Fatal(err)
			}
		}, ErrPoolUnavailable, "build", "pve"},
		// UpdateManagedPool не опускает max_actors ниже резервирований: такой
		// пул — рассогласование базы, и запуск его не принимает.
		{"pool shrunk below the graph", func(t *testing.T, s *Store) { execFit(t, s, `UPDATE managed_pools SET max_actors=1 WHERE uid='pool'`) }, ErrPoolCapacity, "review", "pve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fitStore(t)
			tc.setup(t, s)
			assertFitError(t, s.CheckSwarmRolesFit(ctx, "one", doc), tc.want, tc.role, tc.pool)
		})
	}
	t.Run("missing swarm", func(t *testing.T) {
		if err := fitStore(t).CheckSwarmRolesFit(ctx, "missing", doc); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing swarm: %v", err)
		}
	})
}

// execFit меняет базу напрямую: так воспроизводятся состояния, которых
// публичные операции не допускают.
func execFit(t *testing.T, s *Store, query string) {
	t.Helper()
	if _, err := s.db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

// TestCheckSwarmRolesFitInCallerTransaction — внутри транзакции запуска
// проверка видит её изменения до фиксации и сама ничего не пишет: запуск
// (#211) сверяет роли с той записью, которую делает.
func TestCheckSwarmRolesFitInCallerTransaction(t *testing.T) {
	ctx := context.Background()
	s := fitStore(t)
	doc := fitDoc("build", "review", "test")
	rollback := errors.New("rollback")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		var before, after int64
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT total_changes()`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := s.CheckSwarmRolesFit(ctx, "one", doc); err != nil {
			t.Fatalf("check before change: %v", err)
		}
		if err := s.conn(ctx).QueryRowContext(ctx, `SELECT total_changes()`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("check wrote %d rows", after-before)
		}
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE managed_pools SET state='draining' WHERE uid='pool'`); err != nil {
			t.Fatal(err)
		}
		assertFitError(t, s.CheckSwarmRolesFit(ctx, "one", doc), ErrPoolUnavailable, "build", "pve")
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if err := s.CheckSwarmRolesFit(ctx, "one", doc); err != nil {
		t.Fatalf("rolled back change leaked: %v", err)
	}
}
