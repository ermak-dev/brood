package store

// FEATURE: Привязка роли роя к существующему актору (#208).
// SPEC: swarm_role_bindings(swarm_id, role, actor_uid, actor_incarnation);
// один актор занимает несколько ролей, актор чужого роя недопустим,
// устаревшая инкарнация отвергается при использовании.
// IMPLEMENTATION: BindSwarmRole пишет привязку со сличением инкарнации,
// SwarmsRoleBindings читает их, использующие пути проверяют свежесть.
// RESULT: IMPLEMENTED — привязки атомарны, ограничены своим роем и живым
// актором; ResolveSwarmRole читает одним снимком и сам по себе ничего не
// авторизует: побочный эффект обязан звать его в своей транзакции.
// STATUS: DONE

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func bindingFixture(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	managedFixture(t, s)
	ctx := context.Background()
	if _, err := s.ReserveManagedActor(ctx, managedRequest("actor", "one", 101)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveManagedActor(ctx, managedRequest("other", "two", 102)); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSwarmRoleBindingOneActorManyRoles — один актор занимает несколько ролей.
func TestSwarmRoleBindingOneActorManyRoles(t *testing.T) {
	ctx := context.Background()
	s := bindingFixture(t)
	if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "review", ActorUID: "actor"}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"}); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.SwarmsRoleBindings(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 {
		t.Fatalf("bindings: %+v", bindings)
	}
	seen := map[string]SwarmRoleBinding{}
	for _, b := range bindings {
		seen[b.Role] = b
	}
	if seen["review"].ActorUID != "actor" || seen["build"].ActorUID != "actor" {
		t.Fatalf("bindings: %+v", bindings)
	}
	if seen["review"].ActorIncarnation != 1 || seen["build"].ActorIncarnation != 1 {
		t.Fatalf("binding must record the current incarnation: %+v", bindings)
	}
}

// TestSwarmRoleBindingRebindRole — повторная привязка роли переносит её на другого актора роя.
func TestSwarmRoleBindingRebindRole(t *testing.T) {
	ctx := context.Background()
	s := bindingFixture(t)
	// Актор «other» стоит в чужом рое: напрямую его привязать нельзя,
	// поэтому переносим его в рой «one» и его atespace вне резервирования пула.
	if _, err := s.db.Exec(`UPDATE managed_actors SET swarm_id='one', atespace_uid='native-one' WHERE uid='other'`); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "other"}); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.SwarmsRoleBindings(ctx, "one")
	if err != nil || len(bindings) != 1 || bindings[0].ActorUID != "other" {
		t.Fatalf("rebind: %+v %v", bindings, err)
	}
}

// TestSwarmRoleBindingRejectsForeignSwarmActor — актор чужого роя недопустим.
func TestSwarmRoleBindingRejectsForeignSwarmActor(t *testing.T) {
	ctx := context.Background()
	s := bindingFixture(t)
	err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "other"})
	if err == nil {
		t.Fatal("foreign swarm actor bound")
	}
	bindings, err := s.SwarmsRoleBindings(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 0 {
		t.Fatalf("foreign binding persisted: %+v", bindings)
	}
}

// TestSwarmRoleBindingRejectsUnknownActorAndBadRole — несуществующий актор
// и некорректная роль отвергаются.
func TestSwarmRoleBindingRejectsUnknownActorAndBadRole(t *testing.T) {
	ctx := context.Background()
	s := bindingFixture(t)
	for _, tc := range []struct {
		name string
		b    SwarmRoleBinding
	}{
		{"unknown actor", SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "missing"}},
		{"empty role", SwarmRoleBinding{SwarmID: "one", Role: "", ActorUID: "actor"}},
		{"bad role", SwarmRoleBinding{SwarmID: "one", Role: "Bad Role", ActorUID: "actor"}},
		{"unknown swarm", SwarmRoleBinding{SwarmID: "missing", Role: "build", ActorUID: "actor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.BindSwarmRole(ctx, tc.b); err == nil {
				t.Fatal("invalid binding accepted")
			}
		})
	}
}

// TestSwarmRoleBindingStaleIncarnationRejected — устаревшая инкарнация
// отвергается при использовании привязки, а не при записи.
func TestSwarmRoleBindingStaleIncarnationRejected(t *testing.T) {
	ctx := context.Background()
	s := bindingFixture(t)
	if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"}); err != nil {
		t.Fatal(err)
	}
	// Инкарнация актора растёт: записанная в привязке устаревает.
	if _, err := s.db.Exec(`UPDATE managed_actors SET incarnation = incarnation + 1 WHERE uid = 'actor'`); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.SwarmsRoleBindings(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings: %+v", bindings)
	}
	// Использование устаревшей привязки обязано отказать, а не молча подставить актора.
	current, err := s.ResolveSwarmRole(ctx, "one", "build")
	if err == nil {
		t.Fatalf("stale binding resolved: %+v", current)
	}
	if !errors.Is(err, ErrSwarmRoleStale) {
		t.Fatalf("stale binding must fail with ErrSwarmRoleStale, got %v", err)
	}
	// Перепривязка актуальной инкарнацией возвращает использование к работе.
	if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"}); err != nil {
		t.Fatal(err)
	}
	if current, err = s.ResolveSwarmRole(ctx, "one", "build"); err != nil || current.ActorUID != "actor" || current.ActorIncarnation != 2 {
		t.Fatalf("rebound resolve: %+v %v", current, err)
	}
}

// setActor меняет строку актора напрямую: так воспроизводятся состояния,
// в которые жизненный цикл приводит актора после привязки.
func setActor(t *testing.T, s *Store, uid, set string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE managed_actors SET `+set+` WHERE uid = ?`, uid); err != nil {
		t.Fatal(err)
	}
}

// TestSwarmRoleBindRejectsInactiveActor — привязать можно только живого
// актора, которому желательно работать; удаляемый, удалённый, ошибочный
// актор и актор с desired=deleted отвергаются без записи.
func TestSwarmRoleBindRejectsInactiveActor(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, set string }{
		{"deleted", "phase='deleted'"},
		{"deleting", "phase='deleting'"},
		{"error", "phase='error'"},
		{"stopped", "phase='stopped'"},
		{"desired deleted", "desired='deleted'"},
		{"desired stopped", "desired='stopped'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := bindingFixture(t)
			setActor(t, s, "actor", tc.set)
			err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"})
			if !errors.Is(err, ErrSwarmRoleInactive) {
				t.Fatalf("bind of %s actor: %v", tc.name, err)
			}
			if bindings, err := s.SwarmsRoleBindings(ctx, "one"); err != nil || len(bindings) != 0 {
				t.Fatalf("inactive binding persisted: %+v %v", bindings, err)
			}
		})
	}
}

// waitingStops — строки актора, чью работающую CT reset или update должны
// остановить, а Stop до PVE ещё не дошёл (#275): стадия stop, фаза running,
// желание running, задачи нет, причина последнего отказа. Update хранится
// как операция start.
var waitingStops = map[string]string{
	"update": "operation='start', stage='stop', upid='', desired='running', phase='running', error='Proxmox reconciliation unavailable; allocation retained'",
	"reset":  "operation='reset', stage='stop', upid='', desired='running', phase='running', error='Proxmox reconciliation unavailable; allocation retained'",
}

// settledRunning — работающий актор без незавершённого изменения.
const settledRunning = "operation='start', stage='done', upid='', desired='running', phase='running', error=''"

// waitingStopRefused — отказ актору, чью остановку ждёт change: типизирован
// как ждущая остановка и как неживой актор и называет её и последний отказ.
func waitingStopRefused(t *testing.T, what, change string, err error) {
	t.Helper()
	says := "actor is waiting to stop its running container for " + change + " (last attempt failed: Proxmox reconciliation unavailable; allocation retained)"
	if !errors.Is(err, ErrSwarmRoleStopPending) || !errors.Is(err, ErrSwarmRoleInactive) || !strings.Contains(fmt.Sprint(err), says) {
		t.Fatalf("%s of an actor waiting to stop for %s: %v, want %q", what, change, err, says)
	}
}

// TestSwarmRoleWaitingStopIsNotLive — актор, чью CT reset или update ещё
// только должны остановить, по фазе работает, но роль не принимает и не
// разрешается: шаг прохода ему откажет, а reset сменит инкарнацию. Тот же
// актор без незавершённого изменения роль берёт и разрешается.
func TestSwarmRoleWaitingStopIsNotLive(t *testing.T) {
	ctx := context.Background()
	for change, waiting := range waitingStops {
		t.Run(change, func(t *testing.T) {
			s := bindingFixture(t)
			setActor(t, s, "actor", settledRunning)
			mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"}))
			setActor(t, s, "actor", waiting)
			_, err := s.ResolveSwarmRole(ctx, "one", "build")
			waitingStopRefused(t, "resolve", change, err)
			waitingStopRefused(t, "bind", change, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "review", ActorUID: "actor"}))
			if bindings, err := s.SwarmsRoleBindings(ctx, "one"); err != nil || len(bindings) != 1 || bindings[0].Role != "build" {
				t.Fatalf("refused binding persisted: %+v %v", bindings, err)
			}
			setActor(t, s, "actor", settledRunning)
			if got, err := s.ResolveSwarmRole(ctx, "one", "build"); err != nil || got.ActorUID != "actor" {
				t.Fatalf("resolve of a settled actor: %+v %v", got, err)
			}
			mustNil(t, s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "review", ActorUID: "actor"}))
		})
	}
}

// TestSwarmRoleBindRejectsInactiveSwarm — рой не в состоянии active
// (удаляется, создаётся) роли не принимает.
func TestSwarmRoleBindRejectsInactiveSwarm(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"provisioning", "deleting", "deleted"} {
		t.Run(state, func(t *testing.T) {
			s := bindingFixture(t)
			if _, err := s.db.Exec(`UPDATE swarms SET state=? WHERE id='one'`, state); err != nil {
				t.Fatal(err)
			}
			err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"})
			if !errors.Is(err, ErrSwarmRoleInactive) {
				t.Fatalf("bind in %s swarm: %v", state, err)
			}
		})
	}
}

// TestSwarmRoleBindForeignIsTyped — чужой рой и чужой atespace отличимы по
// типу ошибки, а не по тексту, и ничего не записывают.
func TestSwarmRoleBindForeignIsTyped(t *testing.T) {
	s := bindingFixture(t)
	err := s.BindSwarmRole(context.Background(), SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "other"})
	if !errors.Is(err, ErrSwarmRoleForeignActor) {
		t.Fatalf("foreign bind: %v", err)
	}
	setActor(t, s, "actor", "atespace_uid='stale'")
	err = s.BindSwarmRole(context.Background(), SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"})
	if !errors.Is(err, ErrSwarmRoleForeignActor) {
		t.Fatalf("bind of an actor in a stale atespace: %v", err)
	}
	if bindings, err := s.SwarmsRoleBindings(context.Background(), "one"); err != nil || len(bindings) != 0 {
		t.Fatalf("foreign binding persisted: %+v %v", bindings, err)
	}
}

// TestSwarmRoleResolveRejectsLifecycleDrift — привязка, действовавшая при
// записи, перестаёт разрешаться, когда актор удаляется, останавливается,
// меняет инкарнацию или рой выходит из active.
func TestSwarmRoleResolveRejectsLifecycleDrift(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, set string
		want      error
	}{
		{"incarnation", "incarnation=incarnation+1", ErrSwarmRoleStale},
		{"deleting", "phase='deleting'", ErrSwarmRoleInactive},
		{"deleted", "phase='deleted'", ErrSwarmRoleInactive},
		{"desired deleted", "desired='deleted'", ErrSwarmRoleInactive},
		{"error", "phase='error'", ErrSwarmRoleInactive},
		{"moved to foreign swarm", "swarm_id='two'", ErrSwarmRoleForeignActor},
		{"stale atespace", "atespace_uid='stale'", ErrSwarmRoleForeignActor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := bindingFixture(t)
			if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"}); err != nil {
				t.Fatal(err)
			}
			if got, err := s.ResolveSwarmRole(ctx, "one", "build"); err != nil || got.ActorIncarnation != 1 {
				t.Fatalf("fresh binding must resolve: %+v %v", got, err)
			}
			setActor(t, s, "actor", tc.set)
			if got, err := s.ResolveSwarmRole(ctx, "one", "build"); !errors.Is(err, tc.want) {
				t.Fatalf("drift %s: %+v %v, want %v", tc.name, got, err, tc.want)
			}
		})
	}
	t.Run("swarm deleting", func(t *testing.T) {
		s := bindingFixture(t)
		if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE swarms SET state='deleting' WHERE id='one'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolveSwarmRole(ctx, "one", "build"); !errors.Is(err, ErrSwarmRoleInactive) {
			t.Fatalf("deleting swarm resolved: %v", err)
		}
	})
	t.Run("unbound role", func(t *testing.T) {
		s := bindingFixture(t)
		if _, err := s.ResolveSwarmRole(ctx, "one", "build"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unbound role: %v", err)
		}
	})
	t.Run("role of another swarm", func(t *testing.T) {
		s := bindingFixture(t)
		if _, err := s.db.Exec(`UPDATE managed_actors SET swarm_id='two' WHERE uid='other'`); err != nil {
			t.Fatal(err)
		}
		if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "two", Role: "build", ActorUID: "other"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolveSwarmRole(ctx, "one", "build"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("binding of swarm two leaked into swarm one: %v", err)
		}
	})
}

// TestSwarmRoleResolveSeesCallerTransaction — внутри транзакции вызывающего
// проверка читает её же состояние: изменение актора в этой транзакции
// видно проверке до фиксации. Это и есть способ, которым побочный эффект
// сверяет роль атомарно с собственной записью; отдельный вызов вне
// транзакции даёт только совет.
func TestSwarmRoleResolveSeesCallerTransaction(t *testing.T) {
	ctx := context.Background()
	s := bindingFixture(t)
	if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"}); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("rollback")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.ResolveSwarmRole(ctx, "one", "build"); err != nil {
			t.Fatalf("resolve before change: %v", err)
		}
		if _, err := s.conn(ctx).ExecContext(ctx, `UPDATE managed_actors SET phase='deleting' WHERE uid='actor'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolveSwarmRole(ctx, "one", "build"); !errors.Is(err, ErrSwarmRoleInactive) {
			t.Fatalf("resolve in the same transaction ignored its change: %v", err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if _, err := s.ResolveSwarmRole(ctx, "one", "build"); err != nil {
		t.Fatalf("rolled back change leaked: %v", err)
	}
}

// TestSwarmRoleBindIsTransactional — привязка внутри откатываемой
// транзакции не остаётся.
func TestSwarmRoleBindIsTransactional(t *testing.T) {
	ctx := context.Background()
	s := bindingFixture(t)
	rollback := errors.New("rollback")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if bindings, err := s.SwarmsRoleBindings(ctx, "one"); err != nil || len(bindings) != 0 {
		t.Fatalf("rolled back binding persisted: %+v %v", bindings, err)
	}
}

// TestSwarmRoleConcurrentBindAroundRetirement — параллельные привязки
// до и после выведения актора. Пока актор жив, каждая параллельная привязка
// принимается с его нынешней инкарнацией; после выведения каждая отвергается
// типизированной ошибкой и не оставляет строки, а прежние привязки перестают
// разрешаться. Фазы разделены, чтобы исход каждой привязки был известен.
func TestSwarmRoleConcurrentBindAroundRetirement(t *testing.T) {
	ctx := context.Background()
	s := bindingFixture(t)
	bindAll := func(roles []string) []error {
		errs := make([]error, len(roles))
		var wg sync.WaitGroup
		for i, role := range roles {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: role, ActorUID: "actor"})
			}()
		}
		wg.Wait()
		return errs
	}
	live := []string{"build", "review", "triage", "docs"}
	for i, err := range bindAll(live) {
		if err != nil {
			t.Fatalf("bind %s to a live actor: %v", live[i], err)
		}
	}
	setActor(t, s, "actor", "phase='deleting', desired='deleted'")
	retired := []string{"deploy", "audit", "release", "triage"}
	for i, err := range bindAll(retired) {
		if !errors.Is(err, ErrSwarmRoleInactive) {
			t.Fatalf("bind %s to a retiring actor: %v", retired[i], err)
		}
	}
	bindings, err := s.SwarmsRoleBindings(ctx, "one")
	if err != nil || len(bindings) != len(live) {
		t.Fatalf("bindings after retirement: %+v %v", bindings, err)
	}
	for _, b := range bindings {
		if b.ActorIncarnation != 1 {
			t.Fatalf("binding records incarnation %d, want 1: %+v", b.ActorIncarnation, b)
		}
		if got, err := s.ResolveSwarmRole(ctx, "one", b.Role); !errors.Is(err, ErrSwarmRoleInactive) {
			t.Fatalf("role %s resolves to a retiring actor: %+v %v", b.Role, got, err)
		}
	}
}

// TestSwarmRoleCheckAndEffectAreOneTransaction — проверка роли и запись
// эффекта в одной транзакции не пропускают гонку: выведение актора из
// эксплуатации, начатое между ними, ждёт фиксации и видит уже записанный
// эффект. Гарантию даёт единственное соединение SQLite (docs/GO.md); тест
// падает, если проверка уйдёт из транзакции эффекта на другое соединение.
func TestSwarmRoleCheckAndEffectAreOneTransaction(t *testing.T) {
	ctx := context.Background()
	s := bindingFixture(t)
	if err := s.BindSwarmRole(ctx, SwarmRoleBinding{SwarmID: "one", Role: "build", ActorUID: "actor"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE effect_log(actor_uid TEXT, phase_at_effect TEXT)`); err != nil {
		t.Fatal(err)
	}
	retired := make(chan struct{})
	err := s.WithTx(ctx, func(ctx context.Context) error {
		b, err := s.ResolveSwarmRole(ctx, "one", "build")
		if err != nil {
			return err
		}
		go func() {
			defer close(retired)
			if _, err := s.db.Exec(`UPDATE managed_actors SET phase='deleting', desired='deleted' WHERE uid='actor'`); err != nil {
				t.Error(err)
			}
		}()
		select {
		case <-retired:
			t.Error("retirement overtook the transaction that checked the role")
		case <-time.After(100 * time.Millisecond):
		}
		_, err = s.conn(ctx).ExecContext(ctx, `INSERT INTO effect_log SELECT uid, phase FROM managed_actors WHERE uid=?`, b.ActorUID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	<-retired
	var phase string
	if err := s.db.QueryRow(`SELECT phase_at_effect FROM effect_log`).Scan(&phase); err != nil || phase == "deleting" {
		t.Fatalf("effect recorded against a retiring actor: %q %v", phase, err)
	}
	if _, err := s.ResolveSwarmRole(ctx, "one", "build"); !errors.Is(err, ErrSwarmRoleInactive) {
		t.Fatalf("after retirement: %v", err)
	}
}
