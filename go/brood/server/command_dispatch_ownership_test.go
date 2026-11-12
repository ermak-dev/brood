package server

// BUG: A master could dispatch work to a worker another master added, or to
// another master, and the lease, which never expires, then locked the owning
// master out of that worker's session and lifecycle (#267).
// SPEC: A master dispatches only to the workers it added through add. Dispatch
// to itself, to another master, to another master's worker and to a worker the
// operator created is refused before any lease, with a reason naming whose
// actor it is, and nothing is delivered. A lease taken before the rule stays
// until an explicit release: its holder keeps show and release of the exact
// request, the operator recovers it through the command API, the owning master
// cannot release it. Landing names only the calling master. The operator, the
// step dispatcher and send are not restricted.
// IMPLEMENTATION: executeDispatch runs masterOwns before it claims the lease.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
)

// recordDeliveries подменяет доставку сообщений гостям: каждая доставка
// записывается как "<uid> <первая строка текста>" и подтверждается.
func recordDeliveries(ts *substrateServer) func() []string {
	var mu sync.Mutex
	var calls []string
	ts.srv.managed.deliverMessage = func(_ context.Context, uid string, _ int64, r relay.MessageRequest) (relay.MessageResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, uid+" "+strings.SplitN(r.Body, "\n", 2)[0])
		return relay.MessageResponse{OK: true, Verdict: "delivered"}, nil
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

// dispatchLease возвращает активную dispatch-аренду на акторе a или
// store.ErrNotFound.
func dispatchLease(t *testing.T, ts *substrateServer, a store.ManagedActor) (store.CoordinationClaim, error) {
	t.Helper()
	sw, err := ts.srv.store.GetSwarm(context.Background(), a.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	return ts.srv.store.ShowDispatchLease(context.Background(), sw.ProjectID, sw.ID, a.UID)
}

func TestMasterDispatchRefusesActorsItDidNotAdd(t *testing.T) {
	ts, _, master, other, theirs, _ := threeSlotMasters(t)
	delivered := recordDeliveries(ts)
	for _, target := range foreignTargets {
		got := guestCommand(ts, master, "dispatch", fmt.Sprintf(`{"to":%q,"message":"task","request_id":"foreign-%s"}`, target.name, target.name))
		if got.OK || !strings.Contains(got.Text, "not authorized") || !strings.Contains(got.Text, target.reason) {
			t.Errorf("dispatch to %s: %+v, want a refusal saying %s", target.name, got, target.reason)
		}
	}
	for _, a := range []store.ManagedActor{master, other, theirs} {
		if claim, err := dispatchLease(t, ts, a); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("refused dispatch left a lease on %s: %+v %v", a.Name, claim, err)
		}
	}
	if got := delivered(); len(got) != 0 {
		t.Fatalf("refused dispatch delivered: %v", got)
	}
	// Отказ вызван владением, а не состоянием цели: master, добавивший
	// worker, назначает ему ту же работу.
	if got := guestCommand(ts, other, "dispatch", `{"to":"theirs","message":"task","request_id":"owned"}`); !got.OK || got.Outcome != "confirmed" {
		t.Fatalf("owning master dispatch: %+v", got)
	}
	if claim, err := dispatchLease(t, ts, theirs); err != nil || claim.Source.UID != other.UID {
		t.Fatalf("owning master lease: %+v %v", claim, err)
	}
	// send правило не трогает: сообщение без аренды доходит до чужого
	// worker и до другого master.
	for _, name := range []string{"theirs", "other"} {
		if got := guestCommand(ts, master, "send", fmt.Sprintf(`{"to":%q,"message":"note"}`, name)); !got.OK {
			t.Errorf("send to %s: %+v", name, got)
		}
	}
	want := []string{theirs.UID + " task", theirs.UID + " note", other.UID + " note"}
	if got := delivered(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("deliveries: %v, want %v", got, want)
	}
}

func TestMasterDispatchRefusesOperatorCreatedWorker(t *testing.T) {
	ts, master, worker := coordinationServerSetup(t)
	delivered := recordDeliveries(ts)
	reason := `"worker" was created by the swarm operator, not added by master "master"`
	if got := guestCommand(ts, master, "dispatch", `{"to":"worker","message":"task","request_id":"operator-worker"}`); got.OK || !strings.Contains(got.Text, reason) {
		t.Fatalf("dispatch to the operator's worker: %+v, want a refusal saying %s", got, reason)
	}
	if claim, err := dispatchLease(t, ts, worker); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused dispatch left a lease: %+v %v", claim, err)
	}
	if got := delivered(); len(got) != 0 {
		t.Fatalf("refused dispatch delivered: %v", got)
	}
}

// TestMasterLandingNamesOnlyItself: landing держит токен проекта, а не
// аренду актора, и имя в нём может быть только именем вызывающего master.
func TestMasterLandingNamesOnlyItself(t *testing.T) {
	ts, _, master, _, _, _ := threeSlotMasters(t)
	sw, err := ts.srv.store.GetSwarm(context.Background(), master.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"theirs", "other"} {
		got := guestCommand(ts, master, "landing", fmt.Sprintf(`{"action":"take","name":%q,"request_id":"land-%s"}`, name, name))
		if got.OK || !strings.Contains(got.Text, "landing name must equal the calling managed master") {
			t.Errorf("landing for %s: %+v", name, got)
		}
	}
	if holder, err := ts.srv.store.ShowLandingToken(context.Background(), sw.ProjectID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused landing took the token: %+v %v", holder, err)
	}
	if got := guestCommand(ts, master, "landing", `{"action":"take","name":"master","request_id":"land-self"}`); !got.OK || !strings.Contains(got.Text, "active=true") {
		t.Fatalf("landing take for itself: %+v", got)
	}
	if got := guestCommand(ts, master, "landing", `{"action":"give","request_id":"land-self"}`); !got.OK || !strings.Contains(got.Text, "active=false") {
		t.Fatalf("landing give for itself: %+v", got)
	}
}

// foreignLeaseSetup пишет аренду master на worker другого master так, как
// её взял бы dispatch до #267, и проверяет, что владелец worker остановить
// его не может и чужую аренду не отпускает.
func foreignLeaseSetup(t *testing.T) (ts *substrateServer, alice *http.Client, master, other, theirs store.ManagedActor) {
	t.Helper()
	ts, alice, master, other, theirs, _ = threeSlotMasters(t)
	ctx := context.Background()
	sw, err := ts.srv.store.GetSwarm(ctx, master.SwarmID)
	if err != nil {
		t.Fatal(err)
	}
	claim := coordinationClaim(master, sw, "before-267")
	claim.Target = store.CoordinationActor{UID: theirs.UID, Generation: theirs.Incarnation}
	claim.PayloadHash = fmt.Sprintf("%x", sha256.Sum256([]byte("task")))
	if _, err := ts.srv.store.ClaimDispatchLease(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if got := guestCommand(ts, other, "stop", `{"name":"theirs"}`); got.OK {
		t.Fatalf("owner stopped a worker under a foreign lease: %+v", got)
	}
	if got := guestCommand(ts, other, "dispatch", `{"to":"theirs","request_id":"before-267","action":"release"}`); got.OK || !strings.Contains(got.Text, "belongs to another request or owner") {
		t.Fatalf("owner released a foreign lease: %+v", got)
	}
	if held, err := dispatchLease(t, ts, theirs); err != nil || held != claim {
		t.Fatalf("foreign lease: %+v %v", held, err)
	}
	return ts, alice, master, other, theirs
}

// expectOwnerStops проверяет, что после освобождения аренды владелец снова
// управляет своим worker.
func expectOwnerStops(t *testing.T, ts *substrateServer, other, theirs store.ManagedActor) {
	t.Helper()
	if claim, err := dispatchLease(t, ts, theirs); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease retained: %+v %v", claim, err)
	}
	if got := guestCommand(ts, other, "stop", `{"name":"theirs"}`); !got.OK || got.Outcome != "accepted" {
		t.Fatalf("owner stop after release: %+v", got)
	}
}

func TestForeignDispatchLeaseStaysUntilExplicitRelease(t *testing.T) {
	t.Run("holder", func(t *testing.T) {
		ts, _, master, other, theirs := foreignLeaseSetup(t)
		delivered := recordDeliveries(ts)
		// Повтор прежнего запроса не отправил бы задачу заново; теперь он
		// отказ владения, а аренда остаётся.
		got := guestCommand(ts, master, "dispatch", `{"to":"theirs","message":"task","request_id":"before-267"}`)
		if reason := `"theirs" is a worker of master "other"`; got.OK || !strings.Contains(got.Text, reason) {
			t.Fatalf("retry to a foreign worker: %+v, want a refusal saying %s", got, reason)
		}
		if _, err := dispatchLease(t, ts, theirs); err != nil {
			t.Fatalf("refused retry dropped the lease: %v", err)
		}
		if got := guestCommand(ts, master, "dispatch", `{"to":"theirs","request_id":"before-267","action":"show"}`); !got.OK {
			t.Fatalf("holder show: %+v", got)
		}
		if got := guestCommand(ts, master, "dispatch", `{"to":"theirs","request_id":"before-267","action":"release"}`); !got.OK {
			t.Fatalf("holder release: %+v", got)
		}
		if got := delivered(); len(got) != 0 {
			t.Fatalf("show, release or a refused retry delivered: %v", got)
		}
		expectOwnerStops(t, ts, other, theirs)
	})
	t.Run("operator", func(t *testing.T) {
		ts, alice, master, other, theirs := foreignLeaseSetup(t)
		call := func(args map[string]any) map[string]any {
			t.Helper()
			r, b, raw := ts.do(alice, "POST", "/api/commands/dispatch", ts.csrf(alice), map[string]any{"swarm_id": master.SwarmID, "arguments": args})
			data, _ := b["data"].(map[string]any)
			if r.StatusCode != http.StatusOK || data == nil {
				t.Fatalf("operator dispatch %v: %d %s", args, r.StatusCode, raw)
			}
			return data
		}
		// Оператор видит чужую аренду без её request_id и отпускает точный.
		shown := call(map[string]any{"to": "theirs", "action": "show", "request_id": "unknown"})
		if shown["request_id"] != "before-267" || shown["source_uid"] != master.UID || shown["target_uid"] != theirs.UID {
			t.Fatalf("operator show: %v", shown)
		}
		if released := call(map[string]any{"to": "theirs", "action": "release", "request_id": "before-267"}); released["active"] != false {
			t.Fatalf("operator release: %v", released)
		}
		expectOwnerStops(t, ts, other, theirs)
	})
}
