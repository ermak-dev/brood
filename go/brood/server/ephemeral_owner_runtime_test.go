package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
)

// BUG: Принятый owner reset ephemeral теряет первый start и обычный bootstrap.
// SPEC: Только положительная точная owner lineage продолжает ручную операцию;
// child holds и старые job bindings не снимаются и новой инкарнации не передаются.
// SOLUTION: Точная owner provenance выбирает продолжение и ordinary bootstrap.
// RESULT: FIXED — один ручной Start, HTTP bootstrap без старого pin; stale scope отказан.
// STATUS: FIXED
func manualEphemeralResetFixture(t *testing.T, started bool) (*dispatchServer, store.ManagedActor, store.SwarmChildTask, *managedFakePVE) {
	t.Helper()
	ctx := context.Background()
	d, a, child := runningEphemeralChildReportFixture(t)
	a, err := d.srv.store.BeginOwnerManagedActorOperation(ctx, a, "manual-ephemeral-reset", "reset", d.master.DelegatorID, func(tx context.Context) error {
		return d.srv.authorizeSwarmOwnerIntent(tx, d.master.DelegatorID, a.SwarmID)
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err = d.srv.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "stop", "absent")
	if err != nil {
		t.Fatal(err)
	}
	a, err = d.srv.store.ConfirmManagedActorReset(ctx, a.UID, a.Revision, a.OperationID, "fresh-manual-owner")
	if err != nil {
		t.Fatal(err)
	}
	a, err = d.srv.store.BindManagedActorTask(ctx, a.UID, a.Revision, a.OperationID, "create", "manual-create-task")
	if err != nil {
		t.Fatal(err)
	}
	a, err = d.srv.store.ConfirmManagedActorStage(ctx, a.UID, a.Revision, a.OperationID, "create", "stopped")
	if err != nil {
		t.Fatal(err)
	}
	if started {
		a, err = d.srv.store.ContinueOwnerManagedActorReset(ctx, a, "manual-first-start", func(context.Context, store.OwnerManagedActorLineage) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	pool, err := d.srv.store.GetManagedPool(ctx, a.PoolUID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := d.srv.managed.registry.pools[pool.Connection].runtime
	runtime.actors = pool.MaxActors
	runtime.cpuMillicores = pool.Resources.CPUMillicores
	runtime.memoryBytes = pool.Resources.MemoryBytes
	runtime.diskGiB = pool.Resources.DiskBytes / gib
	provider := &managedFakePVE{server: d.srv, exists: true, spec: d.srv.managed.createSpec(a)}
	d.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) { return provider, nil }
	delete(d.srv.managed.clients, a.PoolUID)
	d.srv.git.remotes[dispatchOrigin] = gitCredential{username: "public-fixture-bot", token: "public-fixture-token"}
	return d, a, child, provider
}

func TestManualEphemeralResetRuntimeContinuesOwnerStart(t *testing.T) {
	ctx := context.Background()
	d, a, child, provider := manualEphemeralResetFixture(t, false)
	d.srv.managed.reconcilePass(ctx, a)
	current, err := d.srv.store.GetManagedActor(ctx, a.UID)
	if err != nil || current.Operation != "start" || current.Stage != "start" || current.UPID != "start-task" || current.Incarnation != 2 {
		t.Fatalf("accepted owner reset was blocked by automatic holds: %+v %v", current, err)
	}
	link, err := d.srv.store.GetDerivedOwnerManagedActorOperation(ctx, current.UID, current.Incarnation, current.OperationID)
	if err != nil || link.Origin.OperationID != "manual-ephemeral-reset" {
		t.Fatalf("runtime start lost owner root: %+v %v", link, err)
	}
	work, err := d.srv.store.SwarmChildWorkByID(ctx, child.ID)
	if err != nil || work.Task != child {
		t.Fatalf("manual reset rewrote old child: %+v %v", work, err)
	}
	if _, err := d.srv.store.GetEphemeralChildIntentForActor(ctx, current.UID, current.Incarnation); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("new manual incarnation adopted original job: %v", err)
	}
	if effects := providerEffectsOf(provider); effects.starts != 1 || effects.creates != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("manual continuation effects: %+v", effects)
	}
}

func TestManualEphemeralResetBootstrapDoesNotAdoptOldJob(t *testing.T) {
	d, a, _, provider := manualEphemeralResetFixture(t, true)
	response := ephemeralBootstrapSourceRequest(d, a, []string{"ephemeral-v1"})
	var body map[string]any
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || body["origin"] != a.Origin || body["mode"] != "gateway" || body["ephemeral"] != nil {
		t.Fatalf("manual incarnation cannot bootstrap without adopting old pass: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("manual bootstrap is cacheable")
	}
	if effects := providerEffectsOf(provider); effects.creates != 0 || effects.starts != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("bootstrap read sent work: %+v", effects)
	}
}

func TestManualEphemeralResetRuntimeRechecksNamespaceAndIntent(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"namespace", "stored-namespace", "new-intent"} {
		t.Run(scenario, func(t *testing.T) {
			d, a, _, provider := manualEphemeralResetFixture(t, false)
			swarm, err := d.srv.store.GetSwarm(ctx, a.SwarmID)
			if err != nil {
				t.Fatal(err)
			}
			var hookErr error
			d.fake.mu.Lock()
			d.fake.beforeNamespaceGet = func() {
				switch scenario {
				case "namespace":
					d.fake.atespaces[swarm.AtespaceName].Metadata.Uid = "replacement"
				case "stored-namespace":
					other := "replacement"
					hookErr = d.srv.store.SetSwarmAtespaceUID(ctx, swarm.ID, &other)
				case "new-intent":
					_, hookErr = d.srv.store.BeginOwnerManagedActorOperation(ctx, a, "new-owner-stop", "stop", d.master.DelegatorID, func(context.Context) error { return nil })
				}
			}
			d.fake.mu.Unlock()
			d.srv.managed.reconcilePass(ctx, a)
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			current, err := d.srv.store.GetManagedActor(ctx, a.UID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "new-intent" {
				if current.OperationID != "new-owner-stop" || current.Operation != "stop" {
					t.Fatalf("late continuation overwrote fresh owner intent: %+v", current)
				}
			} else if current.Operation != "reset" {
				t.Fatalf("changed namespace started owner continuation: %+v", current)
			}
			if effects := providerEffectsOf(provider); effects.starts != 0 || effects.creates != 0 || effects.stops != 0 || effects.deletes != 0 {
				t.Fatalf("changed scope sent work: %+v", effects)
			}
		})
	}
}

func TestManualEphemeralResetBootstrapRechecksAfterNamespaceRead(t *testing.T) {
	d, a, _, provider := manualEphemeralResetFixture(t, true)
	reads := 0
	d.fake.mu.Lock()
	d.fake.beforeNamespaceGet = func() {
		reads++
		createSQL(t, d, `UPDATE managed_actors SET operation_id='changed-start',revision=revision+1 WHERE uid=?`, a.UID)
	}
	d.fake.mu.Unlock()
	response := ephemeralBootstrapSourceRequest(d, a, []string{"ephemeral-v1"})
	if reads != 1 || response.Code < 400 || strings.Contains(response.Body.String(), a.Origin) {
		t.Fatalf("bootstrap released stale manual source: reads=%d status=%d body=%s", reads, response.Code, response.Body.String())
	}
	if effects := providerEffectsOf(provider); effects.starts != 0 || effects.creates != 0 || effects.stops != 0 || effects.deletes != 0 {
		t.Fatalf("bootstrap recheck sent work: %+v", effects)
	}
}

func TestUnrecordedEphemeralIncarnationCannotUseManualBootstrap(t *testing.T) {
	d, _, a := ephemeralBootstrapSourceFixture(t)
	createSQL(t, d, `UPDATE managed_actors SET incarnation=2,operation_id='unrecorded-start',operation='start',stage='start',phase='provisioning' WHERE uid=?`, a.UID)
	current, err := d.srv.store.GetManagedActor(context.Background(), a.UID)
	if err != nil {
		t.Fatal(err)
	}
	response := ephemeralBootstrapSourceRequest(d, current, []string{"ephemeral-v1"})
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing provenance became manual bootstrap: %d %s", response.Code, response.Body.String())
	}
}
