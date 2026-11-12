package server

// BUG: A master could stop and delete actors it did not add (#152).
// SPEC: Every guest command that changes another actor (stop, delete, start,
// reset, restart, wipe, recycle, slash, terminal input) is refused to a master
// for itself, for another master and for any worker it did not add, with a
// reason naming whose actor it is; nothing reaches the provider or the session.
// The master that added the worker keeps every command; reads of peers stay
// open. The operator managing the swarm (command API, panel) and the step
// dispatcher are not restricted.
// IMPLEMENTATION: One ownership check in authorizeCommandTarget for mutations,
// shared by the lifecycle and session executors.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// foreignTargets перечисляет акторов threeSlotMasters, которыми master
// "master" не управляет, и причину отказа, которую видит модель.
var foreignTargets = []struct{ name, reason string }{
	{"theirs", `"theirs" is a worker of master "other"`},
	{"other", `"other" is another master`},
	{"master", `master "master" does not manage itself`},
}

// lifecycleTargetArguments задаёт аргументы команды жизненного цикла для цели %q.
var lifecycleTargetArguments = map[string]string{
	"stop":   `{"name":%q}`,
	"delete": `{"name":%q,"confirm":true}`,
	"start":  `{"name":%q}`,
	"reset":  `{"name":%q,"confirm":true}`,
	"update": `{"name":%q}`,
}

// sessionTargetArguments задаёт аргументы изменяющей сессионной команды для
// цели %q.
var sessionTargetArguments = map[string]string{
	"restart":          `{"name":%q,"confirm":true}`,
	"wipe":             `{"name":%q,"confirm":true}`,
	"recycle":          `{"name":%q,"confirm":true}`,
	"slash":            `{"name":%q,"command":"/clear"}`,
	"session_terminal": `{"name":%q,"action":"input","tid":"t1","data":"x"}`,
}

var sessionControlTools = []string{"restart", "wipe", "recycle", "slash", "session_terminal"}

// ownedWorkerSetup заводит master и worker, добавленный им через add: сессией
// и workspace master управляет только у такого worker.
func ownedWorkerSetup(t *testing.T) (*substrateServer, store.ManagedActor, store.ManagedActor) {
	t.Helper()
	ts, _, master, pve := masterSetup(t)
	return ts, master, lifecycleWorker(t, ts, master, pve)
}

// guestCommand выполняет команду каталога от актора from полным гостевым путём
// relay.
func guestCommand(ts *substrateServer, from store.ManagedActor, tool, args string) relay.SwarmResponse {
	return ts.srv.managed.handleSwarmRequest(context.Background(), from.UID, from.Incarnation, relay.SwarmRequest{Tool: tool, Arguments: json.RawMessage(args)})
}

// recordSessions подменяет сессии гостей: каждый вызов записывается как
// "<uid> <operation>" и подтверждается.
func recordSessions(ts *substrateServer) func() []string {
	var mu sync.Mutex
	var calls []string
	ts.srv.managed.hub = commandSessionHub{guestRelay: ts.srv.managed.hub, call: func(_ context.Context, uid string, _ int64, r relay.SessionRequest) (relay.SessionResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, uid+" "+r.Operation)
		return relay.SessionResponse{OK: true, Text: "done", Outcome: "complete"}, nil
	}}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func TestOwnershipRefusal(t *testing.T) {
	master := store.ManagedActor{UID: "m", Name: "master", Role: "master"}
	for _, c := range []struct {
		target      store.ManagedActor
		owner, want string
	}{
		{store.ManagedActor{UID: "w", Name: "mine", Role: "worker", ParentUID: "m"}, "master", ""},
		{master, "", `actor command not authorized: master "master" does not manage itself; the swarm operator does`},
		{store.ManagedActor{UID: "o", Name: "other", Role: "master"}, "", `actor command not authorized: "other" is another master; a master manages only the workers it added`},
		{store.ManagedActor{UID: "w", Name: "worker", Role: "worker"}, "", `actor command not authorized: "worker" was created by the swarm operator, not added by master "master"; a master manages only the workers it added`},
		{store.ManagedActor{UID: "t", Name: "theirs", Role: "worker", ParentUID: "o"}, "other", `actor command not authorized: "theirs" is a worker of master "other"; a master manages only the workers it added`},
		{store.ManagedActor{UID: "t", Name: "theirs", Role: "worker", ParentUID: "gone"}, "", `actor command not authorized: "theirs" is a worker of another master; a master manages only the workers it added`},
	} {
		if got := ownershipRefusal(master, c.target, c.owner); got != c.want {
			t.Errorf("%s: %q, want %q", c.target.Name, got, c.want)
		}
	}
	// Пустой UID вызывающего не совпадает с пустым ParentUID worker оператора.
	if got := ownershipRefusal(store.ManagedActor{Name: "anonymous", Role: "master"}, store.ManagedActor{UID: "w", Name: "worker", Role: "worker"}, ""); got == "" {
		t.Fatal("an empty caller UID owns the operator's worker")
	}
}

func TestMasterLifecycleRefusesActorsItDidNotAdd(t *testing.T) {
	ts, _, master, other, theirs, pve := threeSlotMasters(t)
	effects := providerEffectsOf(pve)
	for _, tool := range []string{"stop", "delete", "start", "reset"} {
		for _, target := range foreignTargets {
			got := guestCommand(ts, master, tool, fmt.Sprintf(lifecycleTargetArguments[tool], target.name))
			if got.OK || !strings.Contains(got.Text, "not authorized") || !strings.Contains(got.Text, target.reason) {
				t.Errorf("%s of %s: %+v, want a refusal saying %s", tool, target.name, got, target.reason)
			}
		}
	}
	for _, a := range []store.ManagedActor{master, other, theirs} {
		expectUnchanged(t, ts, pve, a, effects)
	}
	// Отказ вызван владением, а не состоянием: master, добавивший worker,
	// тот же stop выполняет.
	if got := guestCommand(ts, other, "stop", `{"name":"theirs"}`); !got.OK || got.Outcome != "accepted" {
		t.Fatalf("owning master stop: %+v", got)
	}
}

func TestMasterSessionControlRefusesActorsItDidNotAdd(t *testing.T) {
	ts, _, master, other, theirs, _ := threeSlotMasters(t)
	calls := recordSessions(ts)
	for _, tool := range sessionControlTools {
		for _, target := range foreignTargets {
			got := guestCommand(ts, master, tool, fmt.Sprintf(sessionTargetArguments[tool], target.name))
			if got.OK || !strings.Contains(got.Text, "not authorized") || !strings.Contains(got.Text, target.reason) {
				t.Errorf("%s of %s: %+v, want a refusal saying %s", tool, target.name, got, target.reason)
			}
		}
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("refused control reached a session: %v", got)
	}
	// Master, добавивший worker, управляет его сессией и workspace.
	var want []string
	for _, tool := range sessionControlTools {
		if got := guestCommand(ts, other, tool, fmt.Sprintf(sessionTargetArguments[tool], "theirs")); !got.OK {
			t.Errorf("owning master %s: %+v", tool, got)
		}
		want = append(want, theirs.UID+" "+tool)
	}
	// Чтение соседей ограничение не трогает.
	for _, target := range []store.ManagedActor{theirs, other} {
		if got := guestCommand(ts, master, "tail", fmt.Sprintf(`{"name":%q}`, target.Name)); !got.OK {
			t.Errorf("tail of %s: %+v", target.Name, got)
		}
		want = append(want, target.UID+" tail")
	}
	if got := calls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session calls: %v, want %v", got, want)
	}
}

func TestMasterRefusesOperatorCreatedWorker(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t)
	calls := recordSessions(ts)
	reason := `"worker" was created by the swarm operator, not added by master "master"`
	for _, call := range [][2]string{{"stop", `{"name":"worker"}`}, {"delete", `{"name":"worker","confirm":true}`}, {"restart", `{"name":"worker","confirm":true}`}, {"slash", `{"name":"worker","command":"/clear"}`}} {
		if got := guestCommand(ts, master, call[0], call[1]); got.OK || !strings.Contains(got.Text, reason) {
			t.Errorf("%s of the operator's worker: %+v", call[0], got)
		}
	}
	after, err := ts.srv.store.GetManagedActor(context.Background(), worker.UID)
	if err != nil || after.Revision != worker.Revision {
		t.Fatalf("refusal changed the worker: %+v %v", after, err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("refused control reached a session: %v", got)
	}
}

// operatorSession выполняет сессионную команду оператора через /api/commands.
func operatorSession(t *testing.T, ts *substrateServer, client *http.Client, swarm, tool, args string) {
	t.Helper()
	var arguments map[string]any
	if err := json.Unmarshal([]byte(args), &arguments); err != nil {
		t.Fatal(err)
	}
	r, _, raw := ts.do(client, "POST", "/api/commands/"+tool, ts.csrf(client), map[string]any{"swarm_id": swarm, "arguments": arguments})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("operator %s %s: %d %s", tool, args, r.StatusCode, raw)
	}
}

func TestOperatorManagesActorsMastersDoNot(t *testing.T) {
	t.Run("master", func(t *testing.T) {
		ts, alice, master, _ := masterSetup(t)
		calls := recordSessions(ts)
		operatorSession(t, ts, alice, master.SwarmID, "slash", `{"name":"master","command":"/clear"}`)
		operatorSession(t, ts, alice, master.SwarmID, "restart", `{"name":"master","confirm":true}`)
		if got := calls(); strings.Join(got, ",") != master.UID+" slash,"+master.UID+" restart" {
			t.Fatalf("operator session control of a master: %v", got)
		}
		if ok, text, data := operatorLifecycle(ts, alice, master.SwarmID)(t, "stop", `{"name":"master"}`); !ok || data["uid"] != master.UID {
			t.Fatalf("operator stop of a master: %s", text)
		}
		settleActor(t, ts, master.UID, actorStopped)
		if ok, text, _ := operatorLifecycle(ts, alice, master.SwarmID)(t, "delete", `{"name":"master","confirm":true}`); !ok {
			t.Fatalf("operator delete of a master: %s", text)
		}
	})
	t.Run("another master's worker", func(t *testing.T) {
		ts, alice, master, other, theirs, _ := threeSlotMasters(t)
		calls := recordSessions(ts)
		operatorSession(t, ts, alice, master.SwarmID, "slash", `{"name":"other","command":"/clear"}`)
		operatorSession(t, ts, alice, master.SwarmID, "wipe", `{"name":"theirs","confirm":true}`)
		if got := calls(); strings.Join(got, ",") != other.UID+" slash,"+theirs.UID+" wipe" {
			t.Fatalf("operator session control: %v", got)
		}
		r, b, _ := ts.do(alice, "POST", "/api/swarms/"+master.SwarmID+"/actors/theirs", ts.csrf(alice), map[string]string{"action": "stop"})
		expectStatus(t, "panel stop", r, b, http.StatusOK)
		settleActor(t, ts, theirs.UID, actorStopped)
		call := operatorLifecycle(ts, alice, master.SwarmID)
		if ok, text, _ := call(t, "start", `{"name":"theirs"}`); !ok {
			t.Fatalf("operator start of another master's worker: %s", text)
		}
		settleActor(t, ts, theirs.UID, actorRunning)
		if ok, text, _ := call(t, "reset", `{"name":"theirs","confirm":true}`); !ok {
			t.Fatalf("operator reset of another master's worker: %s", text)
		}
	})
}

// TestStepDispatcherIgnoresMasterOwnership: диспетчер шагов подтверждает
// workspace и доставляет инструкцию worker, добавленному master, и самому
// master: ограничение гостевых команд его не касается.
func TestStepDispatcherIgnoresMasterOwnership(t *testing.T) {
	d := newDispatchServer(t)
	d.exec(`UPDATE managed_actors SET parent_uid=? WHERE uid=?`, d.master.UID, d.worker.UID)
	d.start("deliver")
	if got := d.complete(d.worker, d.last(), "success"); !got.OK {
		t.Fatalf("completion: %+v", got)
	}
	attests, messages := d.calls()
	if len(attests) != 2 || len(messages) != 2 || attests[0].UID != d.worker.UID || messages[0].UID != d.worker.UID || attests[1].UID != d.master.UID || messages[1].UID != d.master.UID {
		t.Fatalf("dispatcher calls: %+v %+v", attests, messages)
	}
}
