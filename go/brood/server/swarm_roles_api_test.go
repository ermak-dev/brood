package server

// FEATURE: HTTP привязки ролей роя к акторам (#235).
// SPEC: GET /api/swarms/{id}/roles, PUT и DELETE /api/swarms/{id}/roles/{role}.
// Читает любая роль роя, меняет управляющий (owner, editor, администратор);
// viewer — 403, постороннему рой скрыт — 404, изменения требуют CSRF.
// Отказы — фиксированные причины и без записи: роль не DNS label, актор
// чужого роя, неживой актор, другая инкарнация, рой не active, исполнение
// идёт. После привязки запуск роя проходит проверку ролей.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

func (f startAPIFixture) roles(client *http.Client, query string) (*http.Response, map[string]any) {
	r, b, _ := f.ts.do(client, "GET", "/api/swarms/"+f.swarm+"/roles"+query, "", nil)
	return r, b
}

func (f startAPIFixture) bind(client *http.Client, role string, body any) (*http.Response, map[string]any) {
	r, b, _ := f.ts.do(client, "PUT", "/api/swarms/"+f.swarm+"/roles/"+role, f.ts.csrf(client), body)
	return r, b
}

func (f startAPIFixture) unbind(client *http.Client, role string) (*http.Response, map[string]any) {
	r, b, _ := f.ts.do(client, "DELETE", "/api/swarms/"+f.swarm+"/roles/"+role, f.ts.csrf(client), nil)
	return r, b
}

// runningActor резервирует managed-актора роя в пуле master прямо в
// хранилище и объявляет его running: fake PVE ведёт один CT, второго
// живого актора через API ему не поднять. Реконсиляция после этого не
// запускается, так что fake-актор Proxmox не трогает.
func (f startAPIFixture) runningActor(t *testing.T, swarm, name string) store.ManagedActor {
	t.Helper()
	ctx := context.Background()
	s := f.ts.srv.store
	sw, err := s.GetSwarm(ctx, swarm)
	if err != nil || sw.AtespaceUID == nil {
		t.Fatalf("swarm %s: %+v %v", swarm, sw, err)
	}
	a, err := s.ReserveManagedActor(ctx, store.ManagedActor{UID: name + "-uid", Name: name, SwarmID: swarm, AtespaceUID: *sw.AtespaceUID, PoolUID: f.master.PoolUID, Resources: f.master.Resources, CTID: f.master.CTID + 1, OwnerNonce: "owner-" + name, OperationID: "create-" + name})
	if err != nil {
		t.Fatal(err)
	}
	a.Phase = "running"
	if a, err = s.UpdateManagedActor(ctx, a, a.Revision); err != nil {
		t.Fatal(err)
	}
	return a
}

// roleItem находит роль в ответе представления ролей.
func roleItem(t *testing.T, body map[string]any, role string) map[string]any {
	t.Helper()
	items, _ := body["roles"].([]any)
	for _, item := range items {
		if m, _ := item.(map[string]any); m["role"] == role {
			return m
		}
	}
	t.Fatalf("role %q not in %v", role, body)
	return nil
}

func expectRoleDetail(t *testing.T, what string, r *http.Response, b map[string]any, status int, detail string) {
	t.Helper()
	expectStatus(t, what, r, b, status)
	expectDetail(t, what, b, detail)
}

func TestSwarmRolesWire(t *testing.T) {
	f := startAPISetup(t)
	life := f.lifeState(t)
	bindReview := map[string]any{"actorUid": f.master.UID}

	r, b := f.roles(f.bob, "")
	expectRoleDetail(t, "outsider roles", r, b, http.StatusNotFound, "swarm not found")
	r, b = f.bind(f.bob, "review", bindReview)
	expectRoleDetail(t, "outsider bind", r, b, http.StatusNotFound, "swarm not found")
	r, b = f.unbind(f.bob, "build")
	expectRoleDetail(t, "outsider unbind", r, b, http.StatusNotFound, "swarm not found")

	r, b, _ = f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/members", f.ts.csrf(f.alice), map[string]string{"username": "bob", "role": "viewer"})
	expectStatus(t, "add viewer", r, b, http.StatusNoContent)
	r, b = f.roles(f.bob, "")
	expectStatus(t, "viewer roles", r, b, http.StatusOK)
	if b["swarmId"] != f.swarm || b["execution"] != "idle" || b["graph"] != nil {
		t.Fatalf("viewer roles: %v", b)
	}
	build := roleItem(t, b, "build")
	if build["state"] != "ready" || build["inGraph"] != false || build["actorUid"] != f.master.UID || build["actorName"] != "master" || build["actorIncarnation"] != float64(f.master.Incarnation) {
		t.Fatalf("build binding: %v", build)
	}
	r, b = f.bind(f.bob, "review", bindReview)
	expectRoleDetail(t, "viewer bind", r, b, http.StatusForbidden, "swarm management denied")
	r, b = f.unbind(f.bob, "build")
	expectRoleDetail(t, "viewer unbind", r, b, http.StatusForbidden, "swarm management denied")

	r, b, _ = f.ts.do(f.alice, "PUT", "/api/swarms/"+f.swarm+"/roles/review", "", bindReview)
	expectStatus(t, "bind without CSRF", r, b, http.StatusForbidden)
	r, b, _ = f.ts.do(f.alice, "DELETE", "/api/swarms/"+f.swarm+"/roles/build", "", nil)
	expectStatus(t, "unbind without CSRF", r, b, http.StatusForbidden)
	if got := f.lifeState(t); got != life {
		t.Fatalf("refusals wrote:\n%s\nwant\n%s", got, life)
	}

	r, b = f.roles(f.alice, "?definition=review")
	expectStatus(t, "catalog roles", r, b, http.StatusOK)
	graph, _ := b["graph"].(map[string]any)
	if graph["pinned"] != false || graph["name"] != "review" || roleItem(t, b, "review")["state"] != "unbound" || roleItem(t, b, "review")["inGraph"] != true {
		t.Fatalf("catalog roles: %v", b)
	}
	start := map[string]any{"goal": startAPIGoal, "definition": "review", "commit": startAPICommit}
	r, b = f.start(f.alice, start)
	expectStatus(t, "start with unbound review", r, b, http.StatusConflict)
	if !strings.Contains(b["detail"].(string), `role "review"`) {
		t.Fatalf("unbound review reason: %v", b)
	}

	r, b, _ = f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/members", f.ts.csrf(f.alice), map[string]string{"username": "bob", "role": "editor"})
	expectStatus(t, "promote editor", r, b, http.StatusNoContent)
	r, b = f.bind(f.bob, "review", bindReview)
	expectStatus(t, "editor bind", r, b, http.StatusOK)
	review := roleItem(t, b, "review")
	if review["state"] != "ready" || review["actorUid"] != f.master.UID || review["actorName"] != "master" || review["actorIncarnation"] != float64(f.master.Incarnation) {
		t.Fatalf("bound review: %v", b)
	}

	r, b = f.start(f.alice, start)
	expectStatus(t, "start after bind", r, b, http.StatusOK)
	if b["execution"] != "running" {
		t.Fatalf("start after bind: %v", b)
	}
	bound := f.lifeState(t)
	r, b = f.bind(f.bob, "build", bindReview)
	expectRoleDetail(t, "rebind while running", r, b, http.StatusConflict, "swarm execution is running or waiting; stop the swarm before changing role bindings")
	r, b = f.unbind(f.bob, "review")
	expectRoleDetail(t, "unbind while running", r, b, http.StatusConflict, "swarm execution is running or waiting; stop the swarm before changing role bindings")
	if got := f.lifeState(t); got != bound {
		t.Fatalf("refused change during the pass wrote:\n%s\nwant\n%s", got, bound)
	}
	r, b = f.roles(f.bob, "")
	expectStatus(t, "pinned roles", r, b, http.StatusOK)
	graph, _ = b["graph"].(map[string]any)
	if b["execution"] != "running" || graph["pinned"] != true || graph["name"] != "review" || roleItem(t, b, "review")["inGraph"] != true || roleItem(t, b, "build")["inGraph"] != false {
		t.Fatalf("pinned roles: %v", b)
	}
	r, b = f.roles(f.bob, "?definition=release")
	expectRoleDetail(t, "another definition of a pinned swarm", r, b, http.StatusConflict, "swarm graph is pinned to another workflow definition")

	r, b = f.stop(f.alice)
	expectStatus(t, "stop", r, b, http.StatusOK)
	r, b = f.unbind(f.bob, "review")
	expectStatus(t, "unbind after stop", r, b, http.StatusOK)
	if review := roleItem(t, b, "review"); review["state"] != "unbound" || review["actorUid"] != nil {
		t.Fatalf("unbound review: %v", b)
	}
	r, b = f.unbind(f.bob, "review")
	expectRoleDetail(t, "unbind unbound role", r, b, http.StatusNotFound, "swarm role is not bound")
}

// TestSwarmRoleRebindWire — перепривязка переносит роль на другого актора
// того же роя; привязка запоминает его нынешнюю инкарнацию.
func TestSwarmRoleRebindWire(t *testing.T) {
	f := startAPISetup(t)
	worker := f.runningActor(t, f.swarm, "worker")
	r, b := f.bind(f.alice, "build", map[string]any{"actorUid": worker.UID, "actorIncarnation": worker.Incarnation})
	expectStatus(t, "rebind", r, b, http.StatusOK)
	if build := roleItem(t, b, "build"); build["actorUid"] != worker.UID || build["actorName"] != "worker" || build["state"] != "ready" {
		t.Fatalf("rebound build: %v", b)
	}
	bindings, err := f.ts.srv.store.SwarmsRoleBindings(context.Background(), f.swarm)
	if err != nil || len(bindings) != 1 || bindings[0].ActorUID != worker.UID || bindings[0].ActorIncarnation != worker.Incarnation {
		t.Fatalf("stored bindings: %+v %v", bindings, err)
	}
}

// TestSwarmRoleBindRefusalsWire — отказы привязки: фиксированная причина,
// свой статус, ни привязок, ни исполнения, ни акторов запись не меняет.
func TestSwarmRoleBindRefusalsWire(t *testing.T) {
	const (
		foreign  = "actor is not an actor of this swarm"
		inactive = "actor is not live: it must be provisioning or running and meant to run"
		stale    = "actor incarnation changed; list the swarm actors and bind again"
		invalid  = "invalid swarm role binding: expected {actorUid, actorIncarnation?}"
	)
	master := func(f startAPIFixture) map[string]any { return map[string]any{"actorUid": f.master.UID} }
	for _, tc := range []struct {
		name   string
		role   string
		body   func(startAPIFixture) any
		setup  func(*testing.T, startAPIFixture) map[string]any
		status int
		detail string
	}{
		{name: "actor of another swarm", setup: func(t *testing.T, f startAPIFixture) map[string]any {
			r, b, _ := f.ts.do(f.alice, "POST", "/api/projects/"+f.project+"/swarms", f.ts.csrf(f.alice), map[string]string{"name": "beta"})
			expectStatus(t, "beta swarm", r, b, http.StatusCreated)
			stranger := f.runningActor(t, b["id"].(string), "stranger")
			return map[string]any{"actorUid": stranger.UID}
		}, status: http.StatusConflict, detail: foreign},
		{name: "unknown actor", body: func(startAPIFixture) any { return map[string]any{"actorUid": "absent"} }, status: http.StatusConflict, detail: foreign},
		{name: "stopped actor", setup: func(t *testing.T, f startAPIFixture) map[string]any {
			setManagedPhase(t, f, "stopped")
			return master(f)
		}, status: http.StatusConflict, detail: inactive},
		{name: "another incarnation", body: func(f startAPIFixture) any {
			return map[string]any{"actorUid": f.master.UID, "actorIncarnation": f.master.Incarnation + 1}
		}, status: http.StatusConflict, detail: stale},
		{name: "swarm not active", setup: func(t *testing.T, f startAPIFixture) map[string]any {
			if err := f.ts.srv.store.SetSwarmState(context.Background(), f.swarm, "error"); err != nil {
				t.Fatal(err)
			}
			return master(f)
		}, status: http.StatusConflict, detail: "swarm is not active"},
		{name: "execution waiting", setup: func(t *testing.T, f startAPIFixture) map[string]any {
			r, b := f.start(f.alice, map[string]any{"goal": startAPIGoal, "definition": "release", "commit": startAPICommit})
			expectStatus(t, "start", r, b, http.StatusOK)
			return master(f)
		}, status: http.StatusConflict, detail: "swarm execution is running or waiting; stop the swarm before changing role bindings"},
		{name: "role not a DNS label", role: "Review", body: func(f startAPIFixture) any { return master(f) }, status: http.StatusBadRequest, detail: "swarm role must be a lowercase DNS label"},
		{name: "role too long", role: strings.Repeat("r", 64), body: func(f startAPIFixture) any { return master(f) }, status: http.StatusBadRequest, detail: "swarm role must be a lowercase DNS label"},
		{name: "no actor", body: func(startAPIFixture) any { return map[string]any{} }, status: http.StatusBadRequest, detail: invalid},
		{name: "empty actor", body: func(startAPIFixture) any { return map[string]any{"actorUid": ""} }, status: http.StatusBadRequest, detail: invalid},
		{name: "actor not a string", body: func(startAPIFixture) any { return map[string]any{"actorUid": 7} }, status: http.StatusBadRequest, detail: invalid},
		{name: "unknown field", body: func(f startAPIFixture) any { return map[string]any{"actorUid": f.master.UID, "actor": "master"} }, status: http.StatusBadRequest, detail: invalid},
		{name: "zero incarnation", body: func(f startAPIFixture) any { return map[string]any{"actorUid": f.master.UID, "actorIncarnation": 0} }, status: http.StatusBadRequest, detail: invalid},
		{name: "negative incarnation", body: func(f startAPIFixture) any { return map[string]any{"actorUid": f.master.UID, "actorIncarnation": -1} }, status: http.StatusBadRequest, detail: invalid},
		{name: "fractional incarnation", body: func(f startAPIFixture) any { return map[string]any{"actorUid": f.master.UID, "actorIncarnation": 1.5} }, status: http.StatusBadRequest, detail: invalid},
		{name: "incarnation as a string", body: func(f startAPIFixture) any { return map[string]any{"actorUid": f.master.UID, "actorIncarnation": "1"} }, status: http.StatusBadRequest, detail: invalid},
		{name: "null incarnation", body: func(f startAPIFixture) any { return map[string]any{"actorUid": f.master.UID, "actorIncarnation": nil} }, status: http.StatusBadRequest, detail: invalid},
		{name: "not an object", body: func(startAPIFixture) any { return []string{"master"} }, status: http.StatusBadRequest, detail: invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startAPISetup(t)
			var body any
			if tc.setup != nil {
				body = tc.setup(t, f)
			}
			if tc.body != nil {
				body = tc.body(f)
			}
			role := tc.role
			if role == "" {
				role = "review"
			}
			before, life := f.executionState(t), f.lifeState(t)
			r, b := f.bind(f.alice, role, body)
			expectRoleDetail(t, tc.name, r, b, tc.status, tc.detail)
			if got := f.executionState(t); got != before {
				t.Fatalf("refused bind changed execution:\n%s\nwant\n%s", got, before)
			}
			if got := f.lifeState(t); got != life {
				t.Fatalf("refused bind wrote:\n%s\nwant\n%s", got, life)
			}
		})
	}
}

// setManagedPhase переводит актора master фикстуры в фазу phase.
func setManagedPhase(t *testing.T, f startAPIFixture, phase string) {
	t.Helper()
	a, err := f.ts.srv.store.GetManagedActor(context.Background(), f.master.UID)
	if err != nil {
		t.Fatal(err)
	}
	a.Phase = phase
	if _, err := f.ts.srv.store.UpdateManagedActor(context.Background(), a, a.Revision); err != nil {
		t.Fatal(err)
	}
}

// TestSwarmRoleBindDuplicateField — неоднозначное тело отвергается, а не
// решается последним ключом.
func TestSwarmRoleBindDuplicateField(t *testing.T) {
	f := startAPISetup(t)
	life := f.lifeState(t)
	req, err := http.NewRequest("PUT", f.ts.http.URL+"/api/swarms/"+f.swarm+"/roles/review", strings.NewReader(fmt.Sprintf(`{"actorUid":"absent","actorUid":%q}`, f.master.UID)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", f.ts.http.URL)
	req.Header.Set("X-CSRFToken", f.ts.csrf(f.alice))
	resp, err := f.alice.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate actorUid: status %d", resp.StatusCode)
	}
	if got := f.lifeState(t); got != life {
		t.Fatalf("duplicate field wrote:\n%s\nwant\n%s", got, life)
	}
}

// TestSwarmRolesViewWire — состояние привязки — вердикт запуска по актору:
// после пересоздания актора (reset) привязка устаревает, запуск отказывает,
// перепривязка новой инкарнацией возвращает роль. Причины отказа чтения
// фиксированы.
func TestSwarmRolesViewWire(t *testing.T) {
	f := startAPISetup(t)
	ctx := context.Background()
	for _, tc := range []struct{ phase, state string }{{"provisioning", "inactive"}, {"stopped", "inactive"}} {
		setManagedPhase(t, f, tc.phase)
		r, b := f.roles(f.alice, "")
		expectStatus(t, tc.phase, r, b, http.StatusOK)
		if got := roleItem(t, b, "build")["state"]; got != tc.state {
			t.Fatalf("%s actor: state %v", tc.phase, got)
		}
	}
	setManagedPhase(t, f, "running")

	r, b, _ := f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/actors/master", f.ts.csrf(f.alice), map[string]string{"action": "reset"})
	expectStatus(t, "reset master", r, b, http.StatusOK)
	for range 6 {
		f.ts.srv.managed.reconcileOnce(ctx)
	}
	reset, err := f.ts.srv.store.GetManagedActor(ctx, f.master.UID)
	if err != nil || reset.Incarnation != f.master.Incarnation+1 || reset.Phase != "running" {
		t.Fatalf("reset master: %+v %v", reset, err)
	}
	r, b = f.roles(f.alice, "")
	expectStatus(t, "stale", r, b, http.StatusOK)
	if got := roleItem(t, b, "build"); got["state"] != "stale" || got["actorIncarnation"] != float64(f.master.Incarnation) {
		t.Fatalf("stale binding: %v", got)
	}
	release := map[string]any{"goal": startAPIGoal, "definition": "release", "commit": startAPICommit}
	r, b = f.start(f.alice, release)
	expectStatus(t, "start with a stale binding", r, b, http.StatusConflict)
	r, b = f.bind(f.alice, "build", map[string]any{"actorUid": f.master.UID, "actorIncarnation": f.master.Incarnation})
	expectRoleDetail(t, "bind the previous incarnation", r, b, http.StatusConflict, "actor incarnation changed; list the swarm actors and bind again")
	r, b = f.bind(f.alice, "build", map[string]any{"actorUid": f.master.UID, "actorIncarnation": reset.Incarnation})
	expectStatus(t, "rebind the new incarnation", r, b, http.StatusOK)
	if got := roleItem(t, b, "build"); got["state"] != "ready" || got["actorIncarnation"] != float64(reset.Incarnation) {
		t.Fatalf("rebound binding: %v", got)
	}
	r, b = f.start(f.alice, release)
	expectStatus(t, "start after rebind", r, b, http.StatusOK)
}

// TestSwarmRolesViewRefusalsWire — отказы чтения ролей графа каталога
// называют фиксированную причину; определение чужого проекта не видно.
func TestSwarmRolesViewRefusalsWire(t *testing.T) {
	f := startAPISetup(t)
	for _, tc := range []struct {
		query, detail string
		status        int
	}{
		{"?definition=absent", "workflow definition is not in the project catalog", http.StatusNotFound},
		{"?definition=foreign", "workflow definition is not in the project catalog", http.StatusNotFound},
		{"?definition=retrying", "workflow graph is not executable", http.StatusUnprocessableEntity},
	} {
		r, b := f.roles(f.alice, tc.query)
		expectRoleDetail(t, tc.query, r, b, tc.status, tc.detail)
	}
}
