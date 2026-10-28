package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Ключи фиксированных пользователей и ролей тестов.
const (
	idAdmin = "u1"
	idAlice = "u2"
	idBob   = "u3"
)

type testEnv struct {
	svc   *Service
	store *memStore
	prov  *fakeProvisioner
	ids   *idSeq
	now   time.Time
}

// idSeq выдаёт заранее известные идентификаторы, чтобы тесты проверяли
// построение atespace_name из ID.
type idSeq struct{ next int }

var idValues = []string{"u1", "u2", "u3", "p1", "s1", "p2", "s2", "s3"}

func (s *idSeq) gen() string {
	s.next++
	return idValues[s.next-1]
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	store := newMemStore()
	prov := newFakeProvisioner()
	ids := &idSeq{}
	env := &testEnv{store: store, prov: prov, ids: ids, now: baseTime}
	env.svc = NewService(store, prov, func() time.Time { return env.now }, ids.gen)
	return env
}

// seedUsers заводит админа и двух обычных пользователей.
func (e *testEnv) seedUsers(t *testing.T) {
	t.Helper()
	for _, u := range []User{
		{ID: idAdmin, Username: "admin", IsAdmin: true},
		{ID: idAlice, Username: "alice"},
		{ID: idBob, Username: "bob"},
	} {
		if err := e.store.CreateUser(u); err != nil {
			t.Fatalf("seed user %s: %v", u.Username, err)
		}
		e.ids.next++
	}
}

// seedProject создаёт проект администратором и добавляет участников.
func (e *testEnv) seedProject(t *testing.T, members ...string) string {
	t.Helper()
	p, err := e.svc.CreateProject(idAdmin, "proj")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, m := range members {
		if err := e.svc.AddMember(idAdmin, p.ID, m); err != nil {
			t.Fatalf("add member %s: %v", m, err)
		}
	}
	return p.ID
}

func mustCreateSwarm(t *testing.T, e *testEnv, userID, projectID, name string) Swarm {
	t.Helper()
	sw, err := e.svc.CreateSwarm(userID, projectID, name)
	if err != nil {
		t.Fatalf("create swarm: %v", err)
	}
	return sw
}

func TestAdminWithoutMembershipCanCreateAndLosesAccessOnDemotion(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	project := e.seedProject(t, idAlice)
	sw, err := e.svc.CreateSwarm(idAdmin, project, "admin-swarm")
	if err != nil || sw.State != StateActive {
		t.Fatalf("admin create without membership: %+v %v", sw, err)
	}
	u, _, _ := e.store.UserByID(idAdmin)
	u.IsAdmin = false
	if err := e.store.UpdateUser(u); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteSwarm(idAdmin, sw.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("demoted nonmember delete: %v", err)
	}
	if saved, _, _ := e.store.SwarmByID(sw.ID); saved.State != StateActive {
		t.Fatalf("unauthorized mutation: %+v", saved)
	}
}

func TestDeleteRechecksAuthorizationBeforeMutation(t *testing.T) {
	for _, who := range []string{"removed owner", "disabled admin", "unknown user"} {
		t.Run(who, func(t *testing.T) {
			e := newEnv(t)
			e.seedUsers(t)
			project := e.seedProject(t, idAlice)
			sw := mustCreateSwarm(t, e, idAlice, project, "alpha")
			actor := idAlice
			switch who {
			case "removed owner":
				if err := e.svc.RemoveMember(idAdmin, project, idAlice); err != nil {
					t.Fatal(err)
				}
			case "disabled admin":
				actor = idAdmin
				u, _, _ := e.store.UserByID(actor)
				u.Disabled = true
				if err := e.store.UpdateUser(u); err != nil {
					t.Fatal(err)
				}
			case "unknown user":
				actor = "missing"
			}
			if err := e.svc.DeleteSwarm(actor, sw.ID); !errors.Is(err, ErrNotAllowed) {
				t.Errorf("delete authorization: %v", err)
			}
			if saved, _, _ := e.store.SwarmByID(sw.ID); saved.State != StateActive {
				t.Errorf("mutated before authorization: %+v", saved)
			}
			if uid, err := e.prov.GetAtespace(sw.AtespaceName); err != nil || uid != sw.AtespaceUID {
				t.Errorf("provider mutated: %q %v", uid, err)
			}
		})
	}
}

func TestCreateRetryStateAndTransferPolicy(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	project := e.seedProject(t, idAlice)
	sw := mustCreateSwarm(t, e, idAlice, project, "alpha")
	if again, err := e.svc.CreateSwarm(idAlice, project, "alpha"); err != nil || again.ID != sw.ID {
		t.Fatalf("active create retry: %+v %v", again, err)
	}
	if err := e.svc.TransferSwarm(idAdmin, sw.ID, idAdmin); err != nil {
		t.Fatalf("transfer to active admin without membership: %v", err)
	}
	sw, _, _ = e.store.SwarmByID(sw.ID)
	sw.State, sw.Retry = StateError, RetryDelete
	if err := e.store.UpdateSwarm(sw); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateSwarm(idAdmin, project, "alpha"); !errors.Is(err, ErrState) {
		t.Fatalf("create resurrected failed deletion: %v", err)
	}
	if err := e.svc.TransferSwarm(idAdmin, sw.ID, idAlice); !errors.Is(err, ErrState) {
		t.Fatalf("transfer nonactive: %v", err)
	}
	if saved, _, _ := e.store.SwarmByID(sw.ID); saved.State != StateError || saved.Retry != RetryDelete || saved.OwnerID != idAdmin {
		t.Fatalf("invalid operation mutated row: %+v", saved)
	}
}

func TestCan(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)
	sw := mustCreateSwarm(t, e, idAlice, proj, "alpha")

	// Отдельный рой в состоянии deleted: не виден никому, даже админу.
	if err := e.store.SaveSwarm(Swarm{
		ID: "gone", ProjectID: proj, OwnerID: idAlice, Name: "gone",
		AtespaceName: "swarm-gone", State: StateDeleted,
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		user    string
		action  Action
		res     Resource
		allowed bool
	}{
		{"admin views project without membership", idAdmin, ActionViewProject, ProjectRes(proj), true},
		{"admin creates swarm", idAdmin, ActionCreateSwarm, ProjectRes(proj), true},
		{"admin views swarm", idAdmin, ActionViewSwarm, SwarmRes(sw.ID), true},
		{"admin manages swarm", idAdmin, ActionManageSwarm, SwarmRes(sw.ID), true},
		{"admin deletes swarm", idAdmin, ActionDeleteSwarm, SwarmRes(sw.ID), true},
		{"admin transfers swarm", idAdmin, ActionTransferOwnership, SwarmRes(sw.ID), true},
		{"member views project", idAlice, ActionViewProject, ProjectRes(proj), true},
		{"member creates swarm", idAlice, ActionCreateSwarm, ProjectRes(proj), true},
		{"member views own swarm", idAlice, ActionViewSwarm, SwarmRes(sw.ID), true},
		{"member manages own swarm", idAlice, ActionManageSwarm, SwarmRes(sw.ID), true},
		{"member deletes own swarm", idAlice, ActionDeleteSwarm, SwarmRes(sw.ID), true},
		{"owner can transfer own swarm with ACL", idAlice, ActionTransferOwnership, SwarmRes(sw.ID), true},
		{"non-member cannot view project", idBob, ActionViewProject, ProjectRes(proj), false},
		{"non-member cannot create swarm", idBob, ActionCreateSwarm, ProjectRes(proj), false},
		{"non-member cannot view swarm", idBob, ActionViewSwarm, SwarmRes(sw.ID), false},
		{"non-member cannot manage swarm", idBob, ActionManageSwarm, SwarmRes(sw.ID), false},
		{"member cannot manage users", idAlice, ActionManageUsers, NilRes(), false},
		{"member cannot manage projects", idAlice, ActionManageProjects, NilRes(), false},
		{"member cannot manage membership", idAlice, ActionManageMembership, NilRes(), false},
		{"admin manages users", idAdmin, ActionManageUsers, NilRes(), true},
		{"admin manages projects", idAdmin, ActionManageProjects, NilRes(), true},
		{"admin manages membership", idAdmin, ActionManageMembership, NilRes(), true},
		{"unknown user denied", "nope", ActionViewProject, ProjectRes(proj), false},
		{"unknown project denied", idAdmin, ActionViewProject, ProjectRes("nope"), false},
		{"unknown swarm denied", idAdmin, ActionViewSwarm, SwarmRes("nope"), false},
		{"deleted swarm invisible to admin", idAdmin, ActionViewSwarm, SwarmRes("gone"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.svc.Can(tc.user, tc.action, tc.res)
			if err != nil {
				t.Fatalf("Can: %v", err)
			}
			if got != tc.allowed {
				t.Fatalf("Can(%s, %s, %+v) = %v, want %v", tc.user, tc.action, tc.res, got, tc.allowed)
			}
		})
	}
}

func TestCanSwarmStates(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)
	active := mustCreateSwarm(t, e, idAlice, proj, "active")

	mk := func(id, state string) Resource {
		t.Helper()
		if err := e.store.SaveSwarm(Swarm{
			ID: id, ProjectID: proj, OwnerID: idAlice, Name: id,
			AtespaceName: "swarm-" + id, State: SwarmState(state),
			CreatedAt: baseTime, UpdatedAt: baseTime,
		}); err != nil {
			t.Fatal(err)
		}
		return SwarmRes(id)
	}
	provisioning := mk("provisioning", string(StateProvisioning))
	deleting := mk("deleting", string(StateDeleting))
	errState := mk("errstate", string(StateError))

	cases := []struct {
		name    string
		res     Resource
		action  Action
		allowed bool
	}{
		// Виден во всех состояниях, кроме deleted.
		{"view provisioning", provisioning, ActionViewSwarm, true},
		{"view deleting", deleting, ActionViewSwarm, true},
		{"view error", errState, ActionViewSwarm, true},
		// Управление — только активным роем.
		{"manage provisioning", provisioning, ActionManageSwarm, false},
		{"manage deleting", deleting, ActionManageSwarm, false},
		{"manage error", errState, ActionManageSwarm, false},
		{"manage active", SwarmRes(active.ID), ActionManageSwarm, true},
		// Удалять можно активный и упавший, нельзя идущий на удаление
		// через Can (прямой повтор делает DeleteSwarm).
		{"delete error", errState, ActionDeleteSwarm, true},
		{"delete active", SwarmRes(active.ID), ActionDeleteSwarm, true},
		{"delete deleting via Can", deleting, ActionDeleteSwarm, false},
		{"delete provisioning", provisioning, ActionDeleteSwarm, false},
		// Передача владения — только активного роя.
		{"owner can transfer active with ACL", SwarmRes(active.ID), ActionTransferOwnership, true},
		{"transfer error", errState, ActionTransferOwnership, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.svc.Can(idAlice, tc.action, tc.res)
			if err != nil {
				t.Fatalf("Can: %v", err)
			}
			if got != tc.allowed {
				t.Fatalf("Can(%s) = %v, want %v", tc.action, got, tc.allowed)
			}
		})
	}
}

func TestCanDisabledUserAlwaysDenied(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)
	sw := mustCreateSwarm(t, e, idAlice, proj, "alpha")

	if err := e.svc.SetUserDisabled(idAdmin, idAlice, true); err != nil {
		t.Fatalf("disable: %v", err)
	}

	cases := []struct {
		name   string
		user   string
		action Action
		res    Resource
	}{
		{"member view project", idAlice, ActionViewProject, ProjectRes(proj)},
		{"member create swarm", idAlice, ActionCreateSwarm, ProjectRes(proj)},
		{"member view own swarm", idAlice, ActionViewSwarm, SwarmRes(sw.ID)},
		{"member manage own swarm", idAlice, ActionManageSwarm, SwarmRes(sw.ID)},
		{"member manage users", idAlice, ActionManageUsers, NilRes()},
		{"member manage projects", idAlice, ActionManageProjects, NilRes()},
		{"member manage membership", idAlice, ActionManageMembership, NilRes()},
	}
	// Блокировка бьёт и админа.
	if err := e.svc.SetUserDisabled(idAdmin, idAdmin, true); err != nil {
		t.Fatalf("disable admin: %v", err)
	}
	cases = append(cases,
		struct {
			name   string
			user   string
			action Action
			res    Resource
		}{"admin view project", idAdmin, ActionViewProject, ProjectRes(proj)},
		struct {
			name   string
			user   string
			action Action
			res    Resource
		}{"admin manage swarm", idAdmin, ActionManageSwarm, SwarmRes(sw.ID)},
		struct {
			name   string
			user   string
			action Action
			res    Resource
		}{"admin manage users", idAdmin, ActionManageUsers, NilRes()},
	)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := e.svc.Can(tc.user, tc.action, tc.res)
			if err != nil {
				t.Fatalf("Can: %v", err)
			}
			if ok {
				t.Fatalf("disabled user %s allowed %s", tc.user, tc.action)
			}
		})
	}
}

func TestRemovedMemberLosesAccessButSwarmRemains(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)
	sw := mustCreateSwarm(t, e, idAlice, proj, "alpha")

	if err := e.svc.RemoveMember(idAdmin, proj, idAlice); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	for _, tc := range []struct {
		name   string
		action Action
		res    Resource
	}{
		{"view project", ActionViewProject, ProjectRes(proj)},
		{"create swarm", ActionCreateSwarm, ProjectRes(proj)},
		{"view swarm", ActionViewSwarm, SwarmRes(sw.ID)},
		{"manage swarm", ActionManageSwarm, SwarmRes(sw.ID)},
		{"delete swarm", ActionDeleteSwarm, SwarmRes(sw.ID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := e.svc.Can(idAlice, tc.action, tc.res)
			if err != nil {
				t.Fatalf("Can: %v", err)
			}
			if ok {
				t.Fatalf("removed member still allowed %s", tc.action)
			}
		})
	}

	// Рой не удалён: админ видит и управляет им.
	got, err := e.svc.GetSwarm(idAdmin, sw.ID)
	if err != nil {
		t.Fatalf("admin get swarm: %v", err)
	}
	if got.ID != sw.ID {
		t.Fatalf("swarm lost after membership removal")
	}
	ok, err := e.svc.Can(idAdmin, ActionManageSwarm, SwarmRes(sw.ID))
	if err != nil || !ok {
		t.Fatalf("admin lost swarm after member removal: ok=%v err=%v", ok, err)
	}
	// Членство в проекте само по себе не открывает чужой рой.
	ok, _ = e.svc.Can(idBob, ActionViewSwarm, SwarmRes(sw.ID))
	if ok {
		t.Fatal("membership alone must not expose another user's swarm")
	}
}

func TestCreateProjectAdminOnly(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)

	if _, err := e.svc.CreateProject(idAlice, "alice-proj"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-admin create project: err=%v, want ErrNotAllowed", err)
	}
	p, err := e.svc.CreateProject(idAdmin, "proj")
	if err != nil {
		t.Fatalf("admin create project: %v", err)
	}
	if p.ID != "p1" {
		t.Fatalf("project id = %q, want p1", p.ID)
	}
	// Дубликат имени — отказ.
	if _, err := e.svc.CreateProject(idAdmin, "proj"); err == nil {
		t.Fatal("duplicate project name accepted")
	}
}

func TestMembershipAdminOnly(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)

	if err := e.svc.AddMember(idAlice, proj, idBob); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-admin add member: err=%v, want ErrNotAllowed", err)
	}
	if err := e.svc.RemoveMember(idAlice, proj, idAlice); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-admin remove member: err=%v, want ErrNotAllowed", err)
	}
	if err := e.svc.AddMember(idAdmin, proj, idBob); err != nil {
		t.Fatalf("admin add member: %v", err)
	}
	// Повторное добавление — отказ.
	if err := e.svc.AddMember(idAdmin, proj, idBob); err == nil {
		t.Fatal("duplicate membership accepted")
	}
	if err := e.svc.RemoveMember(idAdmin, proj, idBob); err != nil {
		t.Fatalf("admin remove member: %v", err)
	}
	// Удаление отсутствующего членства — отказ.
	if err := e.svc.RemoveMember(idAdmin, proj, idBob); err == nil {
		t.Fatal("removal of missing membership accepted")
	}
	// Членство несуществующего пользователя — отказ.
	if err := e.svc.AddMember(idAdmin, proj, "ghost"); err == nil {
		t.Fatal("membership for unknown user accepted")
	}
}

func TestCreateSwarmFlow(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)

	sw, err := e.svc.CreateSwarm(idAlice, proj, "alpha")
	if err != nil {
		t.Fatalf("create swarm: %v", err)
	}
	if sw.ID != "s1" {
		t.Fatalf("id = %q, want s1", sw.ID)
	}
	if sw.AtespaceName != "swarm-s1" {
		t.Fatalf("atespace name = %q, want swarm-s1", sw.AtespaceName)
	}
	if sw.State != StateActive {
		t.Fatalf("state = %s, want active", sw.State)
	}
	if sw.AtespaceUID == "" {
		t.Fatal("atespace uid not saved")
	}
	if sw.OwnerID != idAlice {
		t.Fatalf("owner = %s, want %s", sw.OwnerID, idAlice)
	}
}

func TestCreateSwarmAtespaceNameFromUUID(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)
	// Генератор выдаёт идентификатор вида UUID: дефисы и верхний регистр.
	uuid := "AbC12345-6789-Def0-1234-567890AbCdEf"
	e.svc = NewService(e.store, e.prov, func() time.Time { return baseTime }, func() string { return uuid })

	sw, err := e.svc.CreateSwarm(idAlice, proj, "alpha")
	if err != nil {
		t.Fatalf("create swarm: %v", err)
	}
	want := "swarm-" + strings.ToLower(strings.ReplaceAll(uuid, "-", ""))
	if sw.AtespaceName != want {
		t.Fatalf("atespace name = %q, want %q", sw.AtespaceName, want)
	}
}

func TestCreateSwarmNonMemberDenied(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)

	if _, err := e.svc.CreateSwarm(idBob, proj, "bob-swarm"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-member create swarm: err=%v, want ErrNotAllowed", err)
	}
	// Удалённый из проекта участник тоже не может.
	if err := e.svc.RemoveMember(idAdmin, proj, idAlice); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateSwarm(idAlice, proj, "alice-swarm"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("removed member create swarm: err=%v, want ErrNotAllowed", err)
	}
	// Заблокированный участник не может и в своём проекте.
	e2 := newEnv(t)
	e2.seedUsers(t)
	proj2 := e2.seedProject(t, idAlice)
	if err := e2.svc.SetUserDisabled(idAdmin, idAlice, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.svc.CreateSwarm(idAlice, proj2, "x"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("disabled member create swarm: err=%v, want ErrNotAllowed", err)
	}
}

func TestCreateSwarmDuplicateNameInProject(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice, idBob)

	if _, err := e.svc.CreateSwarm(idAlice, proj, "alpha"); err != nil {
		t.Fatal(err)
	}
	// Другой участник не может занять то же имя в проекте.
	if _, err := e.svc.CreateSwarm(idBob, proj, "alpha"); err == nil {
		t.Fatal("duplicate swarm name in project accepted")
	}
	// То же имя в другом проекте — можно.
	proj2, err := e.svc.CreateProject(idAdmin, "two")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AddMember(idAdmin, proj2.ID, idBob); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateSwarm(idBob, proj2.ID, "alpha"); err != nil {
		t.Fatalf("same name in another project: %v", err)
	}
}

func TestCreateSwarmProvisionFailure(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)

	e.prov.failCreate = true
	e.prov.createErr = errors.New("substrate down")
	sw, err := e.svc.CreateSwarm(idAlice, proj, "alpha")
	if !errors.Is(err, e.prov.createErr) {
		t.Fatalf("create must return provisioning cause: %v", err)
	}
	if sw.State != StateError {
		t.Fatalf("state = %s, want error", sw.State)
	}
	if sw.AtespaceUID != "" {
		t.Fatal("uid must be empty on failure")
	}
	if sw.Retry != RetryCreate {
		t.Fatalf("retry op = %q, want %q", sw.Retry, RetryCreate)
	}
	saved, _, err := e.store.SwarmByID(sw.ID)
	if err != nil || saved.State != StateError {
		t.Fatalf("error state not persisted: %+v err=%v", saved, err)
	}

	// Повтор: substrate поднялся, тот же вызов продолжает создание.
	e.prov.failCreate = false
	sw2, err := e.svc.CreateSwarm(idAlice, proj, "alpha")
	if err != nil {
		t.Fatalf("retry create: %v", err)
	}
	if sw2.ID != sw.ID {
		t.Fatalf("retry made new swarm %s, want %s", sw2.ID, sw.ID)
	}
	if sw2.State != StateActive || sw2.AtespaceUID == "" {
		t.Fatalf("retry result: state=%s uid=%q", sw2.State, sw2.AtespaceUID)
	}
	if sw2.Retry != "" {
		t.Fatalf("retry op not cleared: %q", sw2.Retry)
	}
}

func TestCreateSwarmRecoversProvisioningRow(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)

	// Рой застрял в provisioning: сбой между записью и вызовом Substrate.
	if err := e.store.SaveSwarm(Swarm{
		ID: "s1", ProjectID: proj, OwnerID: idAlice, Name: "alpha",
		AtespaceName: "swarm-s1", State: StateProvisioning,
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}); err != nil {
		t.Fatal(err)
	}
	sw, err := e.svc.CreateSwarm(idAlice, proj, "alpha")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if sw.ID != "s1" || sw.State != StateActive {
		t.Fatalf("resume result: %+v", sw)
	}
}

func TestRetryCreateRejectsForeignAtespace(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)

	// Рой в error после сбоя провижининга.
	e.prov.failCreate = true
	e.prov.createErr = errors.New("down")
	sw, err := e.svc.CreateSwarm(idAlice, proj, "alpha")
	if !errors.Is(err, e.prov.createErr) {
		t.Fatalf("missing provisioning cause: %v", err)
	}

	// Чужой atespace занял имя до повтора.
	e.prov.failCreate = false
	e.prov.byName[sw.AtespaceName] = "foreign-uid"

	sw2, err := e.svc.CreateSwarm(idAlice, proj, "alpha")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("retry must return foreign UID conflict: %v", err)
	}
	if sw2.State != StateError {
		t.Fatalf("state = %s, want error after foreign atespace", sw2.State)
	}
	if sw2.AtespaceUID != "" {
		t.Fatal("foreign uid must not be adopted")
	}
	if sw2.Retry != RetryCreate {
		t.Fatalf("retry op = %q, want %q", sw2.Retry, RetryCreate)
	}
}

func TestDeleteSwarm(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice, idBob)
	sw := mustCreateSwarm(t, e, idAlice, proj, "alpha")

	// Чужой участник проекта не может удалить.
	if err := e.svc.DeleteSwarm(idBob, sw.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("foreign delete: err=%v, want ErrNotAllowed", err)
	}

	if err := e.svc.DeleteSwarm(idAlice, sw.ID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	saved, _, err := e.store.SwarmByID(sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != StateDeleted {
		t.Fatalf("state = %s, want deleted", saved.State)
	}
	// Atespace в substrate удалён.
	if _, err := e.prov.GetAtespace(saved.AtespaceName); !errors.Is(err, ErrNotFound) {
		t.Fatalf("atespace not deleted: %v", err)
	}
	// Повторное удаление идемпотентно для владельца и админа.
	if err := e.svc.DeleteSwarm(idAlice, sw.ID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if err := e.svc.DeleteSwarm(idAdmin, sw.ID); err != nil {
		t.Fatalf("admin idempotent delete: %v", err)
	}
	// Удалённый рой не виден и не выдаётся.
	if _, err := e.svc.GetSwarm(idAdmin, sw.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted swarm visible: %v", err)
	}
}

func TestDeleteSwarmProvisionerFailure(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)
	sw := mustCreateSwarm(t, e, idAlice, proj, "alpha")

	e.prov.failDelete = true
	e.prov.deleteErr = errors.New("kube down")
	if err := e.svc.DeleteSwarm(idAlice, sw.ID); !errors.Is(err, e.prov.deleteErr) {
		t.Fatalf("delete must return provisioning cause: %v", err)
	}
	saved, _, _ := e.store.SwarmByID(sw.ID)
	if saved.State != StateError {
		t.Fatalf("state = %s, want error", saved.State)
	}
	if saved.Retry != RetryDelete {
		t.Fatalf("retry op = %q, want %q", saved.Retry, RetryDelete)
	}

	// Повтор после восстановления завершает удаление.
	e.prov.failDelete = false
	if err := e.svc.DeleteSwarm(idAlice, sw.ID); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	saved, _, _ = e.store.SwarmByID(sw.ID)
	if saved.State != StateDeleted {
		t.Fatalf("state = %s, want deleted", saved.State)
	}
	if _, err := e.prov.GetAtespace(sw.AtespaceName); !errors.Is(err, ErrNotFound) {
		t.Fatalf("atespace not deleted on retry: %v", err)
	}
}

func TestDeleteSwarmUIDMismatch(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)
	sw := mustCreateSwarm(t, e, idAlice, proj, "alpha")

	// Atespace пересоздан с другим UID: чужой удалять нельзя.
	e.prov.byName[sw.AtespaceName] = "other-uid"

	if err := e.svc.DeleteSwarm(idAlice, sw.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete must return UID conflict: %v", err)
	}
	saved, _, _ := e.store.SwarmByID(sw.ID)
	if saved.State != StateError {
		t.Fatalf("state = %s, want error on uid mismatch", saved.State)
	}
	// Atespace не тронут.
	if _, err := e.prov.GetAtespace(sw.AtespaceName); err != nil {
		t.Fatalf("atespace wrongly deleted: %v", err)
	}
}

func TestDeleteSwarmStateGuards(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)

	if err := e.store.SaveSwarm(Swarm{
		ID: "s1", ProjectID: proj, OwnerID: idAlice, Name: "x",
		AtespaceName: "swarm-s1", State: StateProvisioning,
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteSwarm(idAlice, "s1"); !errors.Is(err, ErrState) {
		t.Fatalf("delete provisioning: err=%v, want ErrState", err)
	}
	if err := e.svc.DeleteSwarm(idAdmin, "s1"); !errors.Is(err, ErrState) {
		t.Fatalf("admin delete provisioning: err=%v, want ErrState", err)
	}
}

func TestTransferSwarm(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice, idBob)
	sw := mustCreateSwarm(t, e, idAlice, proj, "alpha")

	// Членство в проекте без owner-роли не разрешает передачу владения.
	if err := e.svc.TransferSwarm(idBob, sw.ID, idAlice); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-owner transfer: err=%v, want ErrNotAllowed", err)
	}
	// Новый владелец должен быть активным участником проекта.
	if err := e.store.CreateUser(User{ID: "u4", Username: "carol"}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.TransferSwarm(idAdmin, sw.ID, "u4"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("transfer to non-member: err=%v, want ErrNotAllowed", err)
	}
	if err := e.store.CreateUser(User{ID: "u5", Username: "dan", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AddMember(idAdmin, proj, "u5"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.TransferSwarm(idAdmin, sw.ID, "u5"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("transfer to disabled member: err=%v, want ErrNotAllowed", err)
	}
	// Заблокированный админ не передаёт; разблокировать может другой
	// путь восстановления, здесь проверяем только отказ.
	if err := e.svc.SetUserDisabled(idAdmin, idAdmin, true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.TransferSwarm(idAdmin, sw.ID, idBob); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("disabled admin transfer: err=%v, want ErrNotAllowed", err)
	}
	// Прямая разблокировка в хранилище: SetUserDisabled заблокированным
	// админом сам себе недоступна, защита последнего админа — #30.
	u, _, _ := e.store.UserByID(idAdmin)
	u.Disabled = false
	if err := e.store.UpdateUser(u); err != nil {
		t.Fatal(err)
	}

	if err := e.svc.TransferSwarm(idAdmin, sw.ID, idBob); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	got, _, _ := e.store.SwarmByID(sw.ID)
	if got.OwnerID != idBob {
		t.Fatalf("owner = %s, want %s", got.OwnerID, idBob)
	}
	// Бывший владелец потерял управление, новый получил.
	ok, _ := e.svc.Can(idAlice, ActionManageSwarm, SwarmRes(sw.ID))
	if ok {
		t.Fatal("old owner still manages swarm")
	}
	ok, _ = e.svc.Can(idBob, ActionManageSwarm, SwarmRes(sw.ID))
	if !ok {
		t.Fatal("new owner cannot manage swarm")
	}
	// Atespace не изменился при передаче.
	if got.AtespaceName != sw.AtespaceName {
		t.Fatalf("atespace name changed on transfer: %q", got.AtespaceName)
	}
}

func TestTransferSwarmUnknown(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	if err := e.svc.TransferSwarm(idAdmin, "nope", idAlice); !errors.Is(err, ErrNotFound) {
		t.Fatalf("transfer unknown swarm: err=%v, want ErrNotFound", err)
	}
}

func TestSetUserDisabled(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)

	if err := e.svc.SetUserDisabled(idAlice, idAdmin, true); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-admin disable: err=%v, want ErrNotAllowed", err)
	}
	if err := e.svc.SetUserDisabled(idAdmin, idAlice, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	u, _, _ := e.store.UserByID(idAlice)
	if !u.Disabled {
		t.Fatal("user not disabled in store")
	}
	if err := e.svc.SetUserDisabled(idAdmin, idAlice, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	u, _, _ = e.store.UserByID(idAlice)
	if u.Disabled {
		t.Fatal("user still disabled")
	}
}

func TestListProjectsFor(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	e.seedProject(t, idAlice)
	proj2, err := e.svc.CreateProject(idAdmin, "two")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AddMember(idAdmin, proj2.ID, idAlice); err != nil {
		t.Fatal(err)
	}

	got, err := e.svc.ListProjectsFor(idAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("alice sees %d projects, want 2", len(got))
	}
	got, err = e.svc.ListProjectsFor(idBob)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("bob sees %d projects, want 0", len(got))
	}
	// Админ видит все проекты без членства.
	got, err = e.svc.ListProjectsFor(idAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("admin sees %d projects, want all 2 without membership", len(got))
	}
	// Заблокированный не видит ничего.
	if err := e.svc.SetUserDisabled(idAdmin, idAlice, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ListProjectsFor(idAlice); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("disabled list projects: err=%v, want ErrNotAllowed", err)
	}
}

func TestListSwarmsFor(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice, idBob)
	a1 := mustCreateSwarm(t, e, idAlice, proj, "a1")
	b1 := mustCreateSwarm(t, e, idBob, proj, "b1")

	got, err := e.svc.ListSwarmsFor(idAlice, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != a1.ID {
		t.Fatalf("alice sees %+v, want only %s", got, a1.ID)
	}
	got, err = e.svc.ListSwarmsFor(idBob, proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != b1.ID {
		t.Fatalf("bob sees %+v, want only %s", got, b1.ID)
	}
	// Членство само по себе не раскрывает чужие рои.
	if err := e.store.CreateUser(User{ID: "u4", Username: "carol"}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AddMember(idAdmin, proj, "u4"); err != nil {
		t.Fatal(err)
	}
	got, err = e.svc.ListSwarmsFor("u4", proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("carol sees %+v, want none", got)
	}
	// Админ без членства получает все рои проекта.
	if all, err := e.svc.ListSwarmsFor(idAdmin, proj); err != nil || len(all) != 2 {
		t.Fatalf("admin list swarms: got=%+v err=%v, want both owners", all, err)
	}
	// Исключённый участник тоже.
	if err := e.svc.RemoveMember(idAdmin, proj, idBob); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ListSwarmsFor(idBob, proj); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("removed member list swarms: err=%v, want ErrNotAllowed", err)
	}
}

func TestAdminLists(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice, idBob)
	mustCreateSwarm(t, e, idAlice, proj, "a1")
	mustCreateSwarm(t, e, idBob, proj, "b1")

	swarms, err := e.svc.AdminSwarms(idAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if len(swarms) != 2 {
		t.Fatalf("admin sees %d swarms, want 2", len(swarms))
	}
	projects, err := e.svc.AdminProjects(idAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 {
		t.Fatalf("admin sees %d projects, want 1", len(projects))
	}
	// Не админ не получает ничего.
	if _, err := e.svc.AdminSwarms(idAlice); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-admin all swarms: err=%v, want ErrNotAllowed", err)
	}
	if _, err := e.svc.AdminProjects(idAlice); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-admin all projects: err=%v, want ErrNotAllowed", err)
	}
}

func TestSwarmStateTransitions(t *testing.T) {
	legal := map[SwarmState][]SwarmState{
		StateProvisioning: {StateActive, StateError},
		StateActive:       {StateDeleting, StateError},
		StateDeleting:     {StateDeleted, StateError},
		StateError:        {StateActive, StateDeleting, StateError, StateDeleted},
		StateDeleted:      {},
	}
	for from, tos := range legal {
		for _, to := range tos {
			if !canTransition(from, to) {
				t.Errorf("transition %s -> %s must be legal", from, to)
			}
		}
	}
	illegal := []struct{ from, to SwarmState }{
		{StateProvisioning, StateProvisioning},
		{StateProvisioning, StateDeleting},
		{StateProvisioning, StateDeleted},
		{StateActive, StateProvisioning},
		{StateActive, StateActive},
		{StateActive, StateDeleted},
		{StateDeleting, StateProvisioning},
		{StateDeleting, StateActive},
		{StateDeleting, StateDeleting},
		{StateError, StateProvisioning},
		{StateDeleted, StateProvisioning},
		{StateDeleted, StateActive},
		{StateDeleted, StateDeleting},
		{StateDeleted, StateError},
		{StateDeleted, StateDeleted},
	}
	for _, tc := range illegal {
		if canTransition(tc.from, tc.to) {
			t.Errorf("transition %s -> %s must be illegal", tc.from, tc.to)
		}
	}
}

func TestCrossUserNegative(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice, idBob)
	a1 := mustCreateSwarm(t, e, idAlice, proj, "a1")

	for _, tc := range []struct {
		name   string
		action Action
	}{
		{"view", ActionViewSwarm},
		{"manage", ActionManageSwarm},
		{"delete", ActionDeleteSwarm},
	} {
		ok, err := e.svc.Can(idBob, tc.action, SwarmRes(a1.ID))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if ok {
			t.Fatalf("bob allowed %s on alice's swarm", tc.name)
		}
	}
	if _, err := e.svc.GetSwarm(idBob, a1.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("bob get swarm: err=%v, want ErrNotAllowed", err)
	}
	if err := e.svc.DeleteSwarm(idBob, a1.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("bob delete swarm: err=%v, want ErrNotAllowed", err)
	}
	if err := e.svc.TransferSwarm(idBob, a1.ID, idBob); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("bob transfer swarm: err=%v, want ErrNotAllowed", err)
	}

	// Админ может удалить чужой рой.
	if err := e.svc.DeleteSwarm(idAdmin, a1.ID); err != nil {
		t.Fatalf("admin delete: %v", err)
	}
}

func TestTimestampsUseClock(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)
	sw := mustCreateSwarm(t, e, idAlice, proj, "alpha")
	if !sw.CreatedAt.Equal(baseTime) {
		t.Fatalf("created_at = %v, want %v", sw.CreatedAt, baseTime)
	}
	if !sw.UpdatedAt.Equal(baseTime) {
		t.Fatalf("updated_at = %v, want %v", sw.UpdatedAt, baseTime)
	}
}

func TestUnknownUsersAndObjects(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	proj := e.seedProject(t, idAlice)

	if _, err := e.svc.CreateProject("ghost", "x"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("ghost create project: err=%v", err)
	}
	if _, err := e.svc.CreateSwarm("ghost", proj, "x"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("ghost create swarm: err=%v", err)
	}
	if _, err := e.svc.CreateSwarm(idAlice, "ghost-proj", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project create swarm: err=%v, want ErrNotFound", err)
	}
	if _, err := e.svc.GetSwarm(idAdmin, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown swarm: err=%v, want ErrNotFound", err)
	}
	if err := e.svc.DeleteSwarm(idAdmin, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown swarm delete: err=%v, want ErrNotFound", err)
	}
}
