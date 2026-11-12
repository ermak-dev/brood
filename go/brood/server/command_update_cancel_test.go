package server

// FEATURE: An update whose start is not yet requested can be canceled (#133).
// SPEC: After the confirmed stop of an update and until Start is requested, no
// Proxmox task of the update is in flight: the container is stopped. A Start
// that keeps failing before any task (403 after a revoked VM.PowerMgmt,
// ownership drift) keeps the update waiting. The operator, the panel and the
// owning master can then stop the actor (it stays stopped, desired stopped,
// with no further Proxmox write), delete it (as a stopped container, without
// a stop task) or reset it (the container is replaced without a stop task).
// start and update are refused with a reason naming the waiting update and the
// last failure, not an unknown outcome. Once Start may have been sent (the
// provisioning mark or an ambiguous Start), stop, delete and reset stay
// refused.
// IMPLEMENTATION: managedWorkloadOperation.Apply settles the never-requested
// start stage from the observed container; the managed fake refuses Start.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// waitingUpdate — update worker ждёт запроса Start: остановка подтверждена,
// а Start отказывает явно (403), задачи нет.
type waitingUpdate struct {
	ts      *substrateServer
	alice   *http.Client
	master  store.ManagedActor
	waiting store.ManagedActor
	pve     *managedFakePVE
	effects providerEffects
}

func waitingOnRefusedStart(t *testing.T) waitingUpdate {
	t.Helper()
	ts, alice, master, worker, pve, _ := updateSetup(t)
	guestReleaseHub(ts, reportsRelease(oldRelease))
	pve.mu.Lock()
	pve.startError = proxmox.ErrPermission
	pve.mu.Unlock()
	if got := operatorUpdate(ts, alice, master.SwarmID)(t, "worker"); !got.ok || got.outcome != "accepted" {
		t.Fatalf("update: %+v", got)
	}
	var a store.ManagedActor
	for range 4 {
		a = passActor(t, ts, worker.UID)
	}
	if !startPending(a) || a.Phase != "stopped" || a.Desired != "running" || a.Error != managedProviderError(proxmox.ErrPermission) {
		t.Fatalf("update not waiting on the refused Start: %+v", a)
	}
	return waitingUpdate{ts: ts, alice: alice, master: master, waiting: a, pve: pve, effects: providerEffectsOf(pve)}
}

// panelLifecycle — те же изменения через API панели: stop и reset —
// POST action, delete — DELETE, start — POST resume.
func panelLifecycle(ts *substrateServer, client *http.Client, swarm string) lifecycleCaller {
	return func(t *testing.T, tool, _ string) (bool, string, map[string]any) {
		t.Helper()
		method, body := "POST", any(map[string]string{"action": tool})
		switch tool {
		case "delete":
			method, body = "DELETE", nil
		case "start":
			body = map[string]string{"action": "resume"}
		}
		r, b, raw := ts.do(client, method, "/api/swarms/"+swarm+"/actors/worker", ts.csrf(client), body)
		return r.StatusCode < 400, raw, b
	}
}

func waitingCallers(u waitingUpdate) map[string]lifecycleCaller {
	callers := lifecycleCallers(u.ts, u.alice, u.master)
	callers["panel"] = panelLifecycle(u.ts, u.alice, u.master.SwarmID)
	return callers
}

// refusalText — причина отказа: detail JSON-ответа HTTP или текст ответа
// гостю как есть.
func refusalText(text string) string {
	var body struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal([]byte(text), &body) == nil && body.Detail != "" {
		return body.Detail
	}
	return text
}

var cancelArguments = map[string]string{"stop": `{"name":"worker"}`, "delete": `{"name":"worker","confirm":true}`, "reset": `{"name":"worker","confirm":true}`, "start": `{"name":"worker"}`}

func TestUpdateWaitingStartIsCanceledByStop(t *testing.T) {
	for _, via := range []string{"operator", "master", "panel"} {
		t.Run(via, func(t *testing.T) {
			u := waitingOnRefusedStart(t)
			if ok, text, _ := waitingCallers(u)[via](t, "stop", cancelArguments["stop"]); !ok {
				t.Fatalf("stop of a waiting update: %s", text)
			}
			// Остановленная CT остаётся остановленной: ни vzstop, ни Start.
			for range 3 {
				a := passActor(t, u.ts, u.waiting.UID)
				if !actorStopped(a) || a.Operation != "stop" || a.Error != "" || a.OperationID == u.waiting.OperationID || a.Incarnation != u.waiting.Incarnation {
					t.Fatalf("stopped actor: %+v", a)
				}
			}
			if got := providerEffectsOf(u.pve); got != u.effects {
				t.Fatalf("stop reached the provider: %+v want %+v", got, u.effects)
			}
		})
	}
}

func TestUpdateWaitingStartIsCanceledByDelete(t *testing.T) {
	for _, via := range []string{"operator", "master", "panel"} {
		t.Run(via, func(t *testing.T) {
			u := waitingOnRefusedStart(t)
			if ok, text, _ := waitingCallers(u)[via](t, "delete", cancelArguments["delete"]); !ok {
				t.Fatalf("delete of a waiting update: %s", text)
			}
			passUntil(t, u.ts, u.waiting.UID, 4, func(a store.ManagedActor) bool { return a.Phase == "deleted" })
			want := u.effects
			want.deletes++
			if got := providerEffectsOf(u.pve); got != want {
				t.Fatalf("delete provider effects: %+v want %+v", got, want)
			}
		})
	}
}

func TestUpdateWaitingStartIsCanceledByReset(t *testing.T) {
	for _, via := range []string{"operator", "master", "panel"} {
		t.Run(via, func(t *testing.T) {
			u := waitingOnRefusedStart(t)
			if ok, text, _ := waitingCallers(u)[via](t, "reset", cancelArguments["reset"]); !ok {
				t.Fatalf("reset of a waiting update: %s", text)
			}
			// Новое поколение стартует: разрешение вернули.
			u.pve.mu.Lock()
			u.pve.startError = nil
			u.pve.mu.Unlock()
			replaced := passUntil(t, u.ts, u.waiting.UID, 8, func(a store.ManagedActor) bool {
				return actorRunning(a) && a.Incarnation == u.waiting.Incarnation+1
			})
			if replaced.Operation != "start" || replaced.CTID != u.waiting.CTID || replaced.OwnerNonce == u.waiting.OwnerNonce {
				t.Fatalf("reset did not replace the container: %+v", replaced)
			}
			want := u.effects
			want.deletes++
			want.creates++
			want.starts++
			want.bootstraps++
			if got := providerEffectsOf(u.pve); got != want {
				t.Fatalf("reset provider effects: %+v want %+v", got, want)
			}
		})
	}
}

// TestUpdateWaitingStartRefusesStartAndUpdate: start и update не отменяют
// ожидание, а отказ называет его и последний отказ Start, а не неизвестный
// исход.
func TestUpdateWaitingStartRefusesStartAndUpdate(t *testing.T) {
	u := waitingOnRefusedStart(t)
	waiting := `actor "worker" update is waiting to start its stopped container (last attempt failed: Proxmox permission denied; allocation retained); reconcile retries the start on every pass; stop, delete or reset cancels it`
	for via, call := range waitingCallers(u) {
		ok, text, _ := call(t, "start", cancelArguments["start"])
		text = refusalText(text)
		if ok || !strings.Contains(text, "start refused: "+waiting) || strings.Contains(text, "reconcile the current allocation") {
			t.Errorf("%s start of a waiting update: %v %s", via, ok, text)
		}
	}
	for via, call := range updateCallers(u.ts, u.alice, u.master) {
		if got := call(t, "worker"); got.ok || !strings.Contains(got.text, "update refused: "+waiting) {
			t.Errorf("%s update of a waiting update: %+v", via, got)
		}
	}
	expectUnchanged(t, u.ts, u.pve, u.waiting, u.effects)
}

// TestUpdateStartMaybeSentFencesLifecycle: Start мог уйти — отметка
// provisioning записана (сервер остановился до ответа) или ответ Start
// потерян. Тогда stop, delete и reset получают отказ, как при неизвестном
// исходе, и ожидания не называют.
func TestUpdateStartMaybeSentFencesLifecycle(t *testing.T) {
	for name, fault := range map[string]func(*testing.T, *substrateServer, *managedFakePVE, string){
		"provisioning mark": func(t *testing.T, ts *substrateServer, _ *managedFakePVE, uid string) {
			execDB(t, ts, `UPDATE managed_actors SET phase='provisioning', revision=revision+1 WHERE uid=?`, uid)
		},
		"start reply lost": func(t *testing.T, ts *substrateServer, pve *managedFakePVE, uid string) {
			pve.mu.Lock()
			pve.uncertainStart = true
			pve.afterStart = func() {
				pve.mu.Lock()
				pve.running = false
				pve.mu.Unlock()
			}
			pve.mu.Unlock()
			if a := passActor(t, ts, uid); a.Phase != "error" || a.Error != managedProviderError(proxmox.ErrUncertain) {
				t.Fatalf("lost Start reply: %+v", a)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ts, alice, master, worker, pve, _ := updateSetup(t)
			guestReleaseHub(ts, reportsRelease(oldRelease))
			if got := operatorUpdate(ts, alice, master.SwarmID)(t, "worker"); !got.ok {
				t.Fatalf("update: %+v", got)
			}
			if a := passActor(t, ts, worker.UID); !startPending(a) || a.Phase != "stopped" {
				t.Fatalf("stop not confirmed: %+v", a)
			}
			fault(t, ts, pve, worker.UID)
			before, err := ts.srv.store.GetManagedActor(context.Background(), worker.UID)
			if err != nil {
				t.Fatal(err)
			}
			effects := providerEffectsOf(pve)
			u := waitingUpdate{ts: ts, alice: alice, master: master}
			for via, call := range waitingCallers(u) {
				for _, change := range []string{"stop", "delete", "reset"} {
					if ok, text, _ := call(t, change, cancelArguments[change]); ok || strings.Contains(text, "is waiting to start") {
						t.Errorf("%s %s after Start may have been sent: %v %s", via, change, ok, text)
					}
				}
			}
			expectUnchanged(t, ts, pve, before, effects)
		})
	}
}

// TestUpdateWaitingStopRacingPassNamesConflict: проход reconcile повторяет
// отказавший Start и меняет актора, пока stop его проверяет. Отказ называет
// конкурентное изменение и повтор, а не сверку аллокации; повтор проходит.
func TestUpdateWaitingStopRacingPassNamesConflict(t *testing.T) {
	u := waitingOnRefusedStart(t)
	raced := false
	u.ts.srv.managed.clients = map[string]proxmoxAPI{}
	u.ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
		return lifecycleInspect{proxmoxAPI: u.pve, inspect: func(ctx context.Context, ref proxmox.OwnedCT) (proxmox.CTConfig, error) {
			if !raced {
				raced = true
				execDB(t, u.ts, `UPDATE managed_actors SET revision=revision+1 WHERE uid=?`, u.waiting.UID)
			}
			return u.pve.InspectOwned(ctx, ref)
		}}, nil
	}
	stop := operatorLifecycle(u.ts, u.alice, u.master.SwarmID)
	ok, text, _ := stop(t, "stop", cancelArguments["stop"])
	if text = refusalText(text); ok || !strings.Contains(text, "changed while the command was checked") || strings.Contains(text, "reconcile the current allocation") {
		t.Fatalf("stop racing a reconcile pass: %v %s", ok, text)
	}
	if ok, text, _ := stop(t, "stop", cancelArguments["stop"]); !ok {
		t.Fatalf("retried stop: %s", text)
	}
}
