package store

// FEATURE: Привязка ролей роя управляющим (#235).
// SPEC: BindSwarmRoleAs и UnbindSwarmRoleAs в одной транзакции записи
// проверяют сессию, рой (active), право управлять роем, исполнение (не
// running и не waiting) и актора (свой рой и atespace, живой, ожидаемая
// инкарнация), и только затем пишут; любой отказ ничего не пишет.
// ListSwarmRoles показывает роли графа снимка или определения каталога и
// привязки с вердиктом запуска по актору.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// startReview — проход начинается с актора роли review.
const startReview = `apiVersion: brood.dev/v1alpha1
name: review
start: check
exit: done
nodes:
  check: {type: actor, role: review, prompt: review}
  done: {type: exit}
edges:
  - {from: check, to: done}
goalGates: [done]
`

// roleManageStore — startStore и ещё: editor e, участник проекта x вне
// роя, работающий актор b роя «one», работающий актор f роя «two»,
// определения review и неисполнимое retrying. Пул вмещает всех акторов.
func roleManageStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s := startStore(t)
	seedUser(t, s, "e", "editor")
	seedUser(t, s, "x", "outsider")
	mustNil(t, s.AddMember(ctx, "one", "e"))
	mustNil(t, s.AddMember(ctx, "one", "x"))
	mustNil(t, s.SetSwarmMember(ctx, "one", "e", "editor"))
	if _, err := s.db.Exec(`UPDATE managed_pools SET max_actors=8, cpu_millicores=8000, memory_bytes=8192, disk_bytes=16384`); err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		uid, swarm string
		ctid       int
	}{{"b", "one", 202}, {"f", "two", 203}} {
		if _, err := s.ReserveManagedActor(ctx, managedRequest(a.uid, a.swarm, a.ctid)); err != nil {
			t.Fatal(err)
		}
		setActor(t, s, a.uid, "phase='running'")
	}
	publish(t, s, "one", map[string]string{
		"review":   startReview,
		"retrying": strings.Replace(startApprove, "name: approve", "name: retrying", 1) + "retry: {maxRetries: 3}\n",
	})
	return s
}

// roleTables — всё, что смена привязки может записать или задеть.
var roleTables = append([]string{"swarms", "swarm_steps", "swarm_requests"}, lifeTables...)

func bindAs(s *Store, user, role, actor string, incarnation int64) (SwarmRoles, error) {
	return s.BindSwarmRoleAs(context.Background(), SwarmRoleAssignment{SwarmID: "one", UserID: user, Role: role, ActorUID: actor, ActorIncarnation: incarnation}, allow)
}

// roleOf находит роль в представлении ролей роя.
func roleOf(t *testing.T, view SwarmRoles, role string) SwarmRoleState {
	t.Helper()
	for _, r := range view.Roles {
		if r.Role == role {
			return r
		}
	}
	t.Fatalf("role %q not in view %+v", role, view.Roles)
	return SwarmRoleState{}
}

// TestBindSwarmRoleAsManagers — owner, editor и администратор привязывают
// роль; перепривязка переносит роль на другого актора с его инкарнацией.
func TestBindSwarmRoleAsManagers(t *testing.T) {
	s := roleManageStore(t)
	for _, user := range []string{"u", "e", "root"} {
		view, err := bindAs(s, user, "review", "a", 0)
		mustNil(t, err)
		got := roleOf(t, view, "review")
		if got.Binding == nil || got.Binding.ActorUID != "a" || got.Binding.ActorIncarnation != 1 || got.ActorName != "a" || got.Err != nil {
			t.Fatalf("%s bound review: %+v", user, got)
		}
	}
	setActor(t, s, "b", "incarnation=3")
	view, err := bindAs(s, "e", "build", "b", 3)
	mustNil(t, err)
	if got := roleOf(t, view, "build"); got.Binding.ActorUID != "b" || got.Binding.ActorIncarnation != 3 || got.ActorName != "b" {
		t.Fatalf("rebound build: %+v", got)
	}
	bindings, err := s.SwarmsRoleBindings(context.Background(), "one")
	mustNil(t, err)
	if len(bindings) != 2 || bindings[0].Role != "build" || bindings[0].ActorUID != "b" || bindings[1].Role != "review" || bindings[1].ActorUID != "a" {
		t.Fatalf("stored bindings: %+v", bindings)
	}
}

// TestBindSwarmRoleAsRefusesWithoutWrite — каждый отказ называет причину
// типом ошибки и ничего не пишет: ни привязки, ни роя, ни акторов.
func TestBindSwarmRoleAsRefusesWithoutWrite(t *testing.T) {
	for _, tc := range []struct {
		name, user, role, actor string
		incarnation             int64
		setup                   string
		authorize               func(context.Context) bool
		want                    error
	}{
		{name: "viewer", user: "v", role: "build", actor: "b", want: ErrSwarmRequestDenied},
		{name: "project member outside the swarm", user: "x", role: "build", actor: "b", want: ErrSwarmRequestDenied},
		{name: "disabled manager", user: "e", role: "build", actor: "b", setup: `UPDATE users SET disabled=1 WHERE id='e'`, want: ErrSwarmRequestDenied},
		{name: "session revoked", user: "u", role: "build", actor: "b", authorize: func(context.Context) bool { return false }, want: ErrSwarmRequestDenied},
		{name: "actor of another swarm", user: "u", role: "build", actor: "f", want: ErrSwarmRoleForeignActor},
		{name: "unknown actor", user: "u", role: "build", actor: "missing", want: ErrSwarmRoleForeignActor},
		{name: "actor in a stale atespace", user: "u", role: "build", actor: "b", setup: `UPDATE managed_actors SET atespace_uid='stale' WHERE uid='b'`, want: ErrSwarmRoleForeignActor},
		{name: "stopped actor", user: "u", role: "build", actor: "b", setup: `UPDATE managed_actors SET phase='stopped' WHERE uid='b'`, want: ErrSwarmRoleInactive},
		{name: "failed actor", user: "u", role: "build", actor: "b", setup: `UPDATE managed_actors SET phase='error' WHERE uid='b'`, want: ErrSwarmRoleInactive},
		{name: "deleting actor", user: "u", role: "build", actor: "b", setup: `UPDATE managed_actors SET phase='deleting' WHERE uid='b'`, want: ErrSwarmRoleInactive},
		{name: "actor meant to be deleted", user: "u", role: "build", actor: "b", setup: `UPDATE managed_actors SET desired='deleted' WHERE uid='b'`, want: ErrSwarmRoleInactive},
		{name: "another incarnation", user: "u", role: "build", actor: "b", incarnation: 2, want: ErrSwarmRoleStale},
		{name: "swarm not active", user: "u", role: "build", actor: "b", setup: `UPDATE swarms SET state='error' WHERE id='one'`, want: ErrSwarmExecutionState},
		{name: "execution running", user: "u", role: "build", actor: "b", setup: `UPDATE swarms SET execution='running' WHERE id='one'`, want: ErrSwarmRoleBusy},
		{name: "execution waiting", user: "u", role: "review", actor: "a", setup: `UPDATE swarms SET execution='waiting' WHERE id='one'`, want: ErrSwarmRoleBusy},
		{name: "role not a DNS label", user: "u", role: "Build", actor: "b", want: ErrSwarmRoleInvalid},
		{name: "empty role", user: "u", role: "", actor: "b", want: ErrSwarmRoleInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := roleManageStore(t)
			if tc.setup != "" {
				if _, err := s.db.Exec(tc.setup); err != nil {
					t.Fatal(err)
				}
			}
			authorize := tc.authorize
			if authorize == nil {
				authorize = allow
			}
			before := dumpTables(t, s, roleTables...)
			_, err := s.BindSwarmRoleAs(context.Background(), SwarmRoleAssignment{SwarmID: "one", UserID: tc.user, Role: tc.role, ActorUID: tc.actor, ActorIncarnation: tc.incarnation}, authorize)
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
			}
			if after := dumpTables(t, s, roleTables...); after != before {
				t.Fatalf("refused bind wrote:\n%s\nwant\n%s", after, before)
			}
		})
	}
}

// TestBindSwarmRoleAsUnknownSwarm — рой, которого нет, — ErrNotFound.
func TestBindSwarmRoleAsUnknownSwarm(t *testing.T) {
	s := roleManageStore(t)
	_, err := s.BindSwarmRoleAs(context.Background(), SwarmRoleAssignment{SwarmID: "absent", UserID: "u", Role: "build", ActorUID: "b"}, allow)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown swarm: %v", err)
	}
}

// TestBindSwarmRoleAsChecksInsideTransaction — сессия перепроверяется в
// транзакции записи: authorize получает её контекст.
func TestBindSwarmRoleAsChecksInsideTransaction(t *testing.T) {
	s := roleManageStore(t)
	// Срез, а не map: снятие требует привязки, сделанной первым случаем.
	for _, tc := range []struct {
		name string
		call func(func(context.Context) bool) error
	}{
		{"bind", func(authorize func(context.Context) bool) error {
			_, err := s.BindSwarmRoleAs(context.Background(), SwarmRoleAssignment{SwarmID: "one", UserID: "u", Role: "review", ActorUID: "a"}, authorize)
			return err
		}},
		{"unbind", func(authorize func(context.Context) bool) error {
			_, err := s.UnbindSwarmRoleAs(context.Background(), "one", "u", "review", authorize)
			return err
		}},
	} {
		inside := false
		err := tc.call(func(ctx context.Context) bool {
			_, inside = ctx.Value(txKey{}).(*sql.Tx)
			return true
		})
		if err != nil || !inside {
			t.Fatalf("%s: authorize inside transaction %v, err %v", tc.name, inside, err)
		}
	}
}

// TestRebindRefusedWhileExecutionRuns — перепривязка и снятие при идущем
// проходе — отказ; после остановки привязка снова меняется.
func TestRebindRefusedWhileExecutionRuns(t *testing.T) {
	ctx := context.Background()
	s := roleManageStore(t)
	w, err := startOne(s, "release")
	mustNil(t, err)
	if w.Execution != "waiting" {
		t.Fatalf("started: %s", w.Execution)
	}
	before := dumpTables(t, s, roleTables...)
	if _, err := bindAs(s, "u", "build", "b", 0); !errors.Is(err, ErrSwarmRoleBusy) {
		t.Fatalf("rebind while waiting: %v", err)
	}
	if _, err := s.UnbindSwarmRoleAs(ctx, "one", "u", "build", allow); !errors.Is(err, ErrSwarmRoleBusy) {
		t.Fatalf("unbind while waiting: %v", err)
	}
	if after := dumpTables(t, s, roleTables...); after != before {
		t.Fatalf("refused change wrote:\n%s\nwant\n%s", after, before)
	}
	_, err = s.StopSwarm(ctx, "one", "u", allow)
	mustNil(t, err)
	view, err := bindAs(s, "u", "build", "b", 0)
	mustNil(t, err)
	if got := roleOf(t, view, "build"); got.Binding.ActorUID != "b" {
		t.Fatalf("rebind after stop: %+v", got)
	}
}

// TestUnbindSwarmRoleAs — снятие удаляет только свою привязку; снятие
// несвязанной роли, отказ прав и неверная роль ничего не пишут.
func TestUnbindSwarmRoleAs(t *testing.T) {
	ctx := context.Background()
	s := roleManageStore(t)
	_, err := bindAs(s, "u", "review", "a", 0)
	mustNil(t, err)
	for _, tc := range []struct {
		name, user, role string
		want             error
	}{
		{"viewer", "v", "build", ErrSwarmRequestDenied},
		{"project member outside the swarm", "x", "build", ErrSwarmRequestDenied},
		{"unbound role", "u", "deploy", ErrSwarmRoleUnbound},
		{"role not a DNS label", "u", "-build", ErrSwarmRoleInvalid},
	} {
		before := dumpTables(t, s, roleTables...)
		if _, err := s.UnbindSwarmRoleAs(ctx, "one", tc.user, tc.role, allow); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if after := dumpTables(t, s, roleTables...); after != before {
			t.Fatalf("%s: refused unbind wrote:\n%s\nwant\n%s", tc.name, after, before)
		}
	}
	view, err := s.UnbindSwarmRoleAs(ctx, "one", "e", "review", allow)
	mustNil(t, err)
	if len(view.Roles) != 1 || view.Roles[0].Role != "build" {
		t.Fatalf("view after unbind: %+v", view.Roles)
	}
	bindings, err := s.SwarmsRoleBindings(ctx, "one")
	mustNil(t, err)
	if len(bindings) != 1 || bindings[0].Role != "build" {
		t.Fatalf("stored bindings after unbind: %+v", bindings)
	}
}

// TestBindThenStartPassesRolesFit — роль графа без привязки не даёт
// запустить рой; после привязки тот же запуск проходит CheckSwarmRolesFit.
func TestBindThenStartPassesRolesFit(t *testing.T) {
	s := roleManageStore(t)
	var fit *RoleFitError
	if _, err := startOne(s, "review"); !errors.As(err, &fit) || fit.Role != "review" || !errors.Is(err, ErrSwarmRoleUnbound) {
		t.Fatalf("start with unbound review: %v", err)
	}
	_, err := bindAs(s, "e", "review", "b", 1)
	mustNil(t, err)
	w, err := startOne(s, "review")
	mustNil(t, err)
	if w.Execution != "running" {
		t.Fatalf("start after bind: %s", w.Execution)
	}
}

// TestListSwarmRoles — роли графа каталога до первого запуска и снимка
// после него, привязки с вердиктом запуска по актору.
func TestListSwarmRoles(t *testing.T) {
	ctx := context.Background()
	s := roleManageStore(t)
	view, err := s.ListSwarmRoles(ctx, "one", "")
	mustNil(t, err)
	if view.Graph != nil || len(view.Roles) != 1 || view.Roles[0].Role != "build" || view.Roles[0].InGraph || view.Roles[0].Err != nil || view.Roles[0].ActorName != "a" {
		t.Fatalf("view without graph: %+v %+v", view.Graph, view.Roles)
	}

	view, err = s.ListSwarmRoles(ctx, "one", "review")
	mustNil(t, err)
	catalog, err := s.GetWorkflowDefinition(ctx, "one", "review")
	mustNil(t, err)
	if g := view.Graph; g == nil || g.Pinned || g.Name != "review" || g.Version != catalog.Version || g.Revision != catalog.Revision || strings.Join(g.Roles, ",") != "review" {
		t.Fatalf("catalog graph: %+v", view.Graph)
	}
	build, review := roleOf(t, view, "build"), roleOf(t, view, "review")
	if build.InGraph || build.Err != nil || !review.InGraph || review.Binding != nil || !errors.Is(review.Err, ErrSwarmRoleUnbound) {
		t.Fatalf("catalog roles: build %+v review %+v", build, review)
	}

	for _, tc := range []struct{ definition string }{{"absent"}, {"foreign"}} {
		if _, err := s.ListSwarmRoles(ctx, "one", tc.definition); !errors.Is(err, ErrWorkflowDefinitionMissing) {
			t.Fatalf("definition %q: %v", tc.definition, err)
		}
	}
	if _, err := s.ListSwarmRoles(ctx, "one", "retrying"); !errors.Is(err, ErrWorkflowGraphInvalid) {
		t.Fatalf("non-executable definition: %v", err)
	}

	_, err = startOne(s, "release")
	mustNil(t, err)
	for _, definition := range []string{"", "release"} {
		view, err = s.ListSwarmRoles(ctx, "one", definition)
		mustNil(t, err)
		if g := view.Graph; g == nil || !g.Pinned || g.Name != "release" || strings.Join(g.Roles, ",") != "build" || !roleOf(t, view, "build").InGraph {
			t.Fatalf("pinned graph for %q: %+v", definition, view.Graph)
		}
	}
	if _, err := s.ListSwarmRoles(ctx, "one", "review"); !errors.Is(err, ErrSwarmGraphPinned) {
		t.Fatalf("another definition of a pinned swarm: %v", err)
	}
	if _, err := s.ListSwarmRoles(ctx, "absent", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown swarm: %v", err)
	}
}

// TestListSwarmRolesVerdicts — вердикт привязки тот же, что дал бы запуск:
// устаревшая инкарнация, неработающий, чужой и исчезнувший актор. Имя
// чужого актора не раскрывается.
func TestListSwarmRolesVerdicts(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, setup string
		want        error
		actorName   string
	}{
		{"ready", "", nil, "a"},
		{"stale", `UPDATE managed_actors SET incarnation=2 WHERE uid='a'`, ErrSwarmRoleStale, "a"},
		{"provisioning", `UPDATE managed_actors SET phase='provisioning' WHERE uid='a'`, ErrSwarmRoleInactive, "a"},
		{"stopped", `UPDATE managed_actors SET phase='stopped' WHERE uid='a'`, ErrSwarmRoleInactive, "a"},
		{"moved to another swarm", `UPDATE managed_actors SET swarm_id='two', atespace_uid='native-two' WHERE uid='a'`, ErrSwarmRoleForeignActor, ""},
		{"gone", `UPDATE swarm_role_bindings SET actor_uid='gone' WHERE role='build'`, ErrSwarmRoleUnbound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := roleManageStore(t)
			if tc.setup != "" {
				if _, err := s.db.Exec(tc.setup); err != nil {
					t.Fatal(err)
				}
			}
			view, err := s.ListSwarmRoles(ctx, "one", "")
			mustNil(t, err)
			got := roleOf(t, view, "build")
			if got.Binding == nil || (tc.want == nil) != (got.Err == nil) || (tc.want != nil && !errors.Is(got.Err, tc.want)) || got.ActorName != tc.actorName {
				t.Fatalf("%s: %+v", tc.name, got)
			}
		})
	}
}
