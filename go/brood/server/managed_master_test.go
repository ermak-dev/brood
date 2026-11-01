package server

// FEATURE: Operator-approved managed master delegation.
// SPEC: Only an active admin can approve a master; workers cannot add. Add inherits
// the approved pool, uses normal admission, and retries retain the exact allocation.
// IMPLEMENTATION: UID-bound master approval and shared managed creation/admission.
// RESULT: IMPLEMENTED — approval, revocation, admission, retries and strict JSON verified.
// STATUS: DONE

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func TestManagedRolePreservesDuplicateFieldRejection(t *testing.T) {
	for _, body := range []string{
		`{"metadata":{"name":"first"},"metadata":{"name":"second"},"role":"worker"}`,
		`{"metadata":{"name":"actor"},"role":"master","role":"worker"}`,
		`{"metadata":{"name":"actor"},"actorTemplate":{"name":"first"},"actorTemplate":{"name":"second"},"role":"worker"}`,
		`{"metadata":{"name":"first","name":"second"},"role":"worker"}`,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		var actor ateapi.Actor
		if role, ok := parseActorCreation(w, r, &actor); ok || w.Code != 400 {
			t.Fatalf("ambiguous actor accepted role=%q status=%d body=%s", role, w.Code, body)
		}
	}
}

func masterSetup(t *testing.T) (*substrateServer, *http.Client, store.ManagedActor, *managedFakePVE) {
	t.Helper()
	ts, alice, _, swarm, pve, hub := managedSetup(t)
	ctx := context.Background()
	u, err := ts.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.srv.store.SetUserAdmin(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "master"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}, "role": "master"})
	expectStatus(t, "master", r, b, 201)
	hub.connected = true
	for range 4 {
		ts.srv.managed.reconcileOnce(ctx)
	}
	a, err := ts.srv.store.LookupManagedActor(ctx, swarm, "master")
	if err != nil {
		t.Fatal(err)
	}
	if a.Phase != "running" || a.DelegatorID != u.ID {
		t.Fatalf("master: %+v", a)
	}
	return ts, alice, a, pve
}

type masterNextID struct {
	proxmoxAPI
	next func(context.Context) (int, error)
}

func (p masterNextID) NextID(ctx context.Context) (int, error) { return p.next(ctx) }

func TestManagedMasterRechecksAfterAllocationLookup(t *testing.T) {
	ts, _, master, pve := masterSetup(t)
	ts.srv.managed.clients = map[string]proxmoxAPI{}
	ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
		return masterNextID{proxmoxAPI: pve, next: func(ctx context.Context) (int, error) {
			return 106, ts.srv.store.SetUserDisabled(ctx, master.DelegatorID, true)
		}}, nil
	}
	got := ts.srv.managed.handleSwarmRequest(context.Background(), master.UID, master.Incarnation, relay.SwarmRequest{Tool: "add", To: "forbidden"})
	if got.OK || pve.creates != 1 {
		t.Fatalf("revocation during lookup accepted: %+v creates=%d", got, pve.creates)
	}
	if _, err := ts.srv.store.LookupManagedActor(context.Background(), master.SwarmID, "forbidden"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked request reserved: %v", err)
	}
}

func TestManagedMasterDoesNotExposeProviderErrors(t *testing.T) {
	ts, _, master, pve := masterSetup(t)
	ts.srv.managed.clients = map[string]proxmoxAPI{}
	ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
		return masterNextID{proxmoxAPI: pve, next: func(context.Context) (int, error) { return 0, errors.New("private-provider-sentinel") }}, nil
	}
	got := ts.srv.managed.handleSwarmRequest(context.Background(), master.UID, master.Incarnation, relay.SwarmRequest{Tool: "add", To: "child"})
	if got.OK || strings.Contains(got.Text, "private-provider-sentinel") {
		t.Fatalf("provider error escaped: %+v", got)
	}
}

func TestManagedMasterAdmissionBoundaries(t *testing.T) {
	for _, scenario := range []string{"quota", "selector", "native-collision", "namespace", "drained", "disabled", "nonrunning", "worker"} {
		t.Run(scenario, func(t *testing.T) {
			ts, alice, master, pve := masterSetup(t)
			expectedCreates := 1
			ctx := context.Background()
			sw, err := ts.srv.store.GetSwarm(ctx, master.SwarmID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "quota":
				one := int64(1)
				err = ts.srv.store.SetProjectCapacity(ctx, sw.ProjectID, store.ProjectCapacity{MaxActors: &one})
			case "selector":
				err = ts.srv.store.SetProjectCapacity(ctx, sw.ProjectID, store.ProjectCapacity{WorkerSelector: map[string]string{"ate.dev/worker-pool": "other"}})
			case "native-collision":
				ts.fake.mu.Lock()
				ts.fake.actors[sw.AtespaceName+"/child"] = &ateapi.Actor{Metadata: fakeMeta("child", sw.AtespaceName)}
				ts.fake.mu.Unlock()
			case "namespace":
				ts.fake.mu.Lock()
				ts.fake.atespaces[sw.AtespaceName].Metadata.Uid = "replaced"
				ts.fake.mu.Unlock()
			case "drained":
				p, e := ts.srv.store.GetManagedPool(ctx, master.PoolUID)
				if e != nil {
					t.Fatal(e)
				}
				p.State = "draining"
				_, err = ts.srv.store.UpdateManagedPool(ctx, p, p.Revision)
			case "disabled":
				err = ts.srv.store.SetUserDisabled(ctx, master.DelegatorID, true)
			case "nonrunning":
				master.Phase = "error"
				master, err = ts.srv.store.UpdateManagedActor(ctx, master, master.Revision)
			case "worker":
				pve.running = false // The single-container fake must start the new CT stopped.
				pve.nextIDs = []int{106}
				r, b, _ := ts.do(alice, "POST", "/api/swarms/"+sw.ID+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "worker"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
				expectStatus(t, "worker", r, b, 201)
				for range 4 {
					ts.srv.managed.reconcileOnce(ctx)
				}
				master, err = ts.srv.store.LookupManagedActor(ctx, sw.ID, "worker")
				if err != nil || master.Phase != "running" || master.Role != "worker" {
					t.Fatalf("worker not running: %+v %v", master, err)
				}
				expectedCreates = 2
			}
			if err != nil {
				t.Fatal(err)
			}
			got := ts.srv.managed.handleSwarmRequest(ctx, master.UID, master.Incarnation, relay.SwarmRequest{Tool: "add", To: "child"})
			if got.OK || pve.creates != expectedCreates {
				t.Fatalf("boundary bypassed: %+v creates=%d", got, pve.creates)
			}
		})
	}
}

func TestManagedMasterApprovalAndAdd(t *testing.T) {
	ts, alice, _, swarm, pve, hub := managedSetup(t)
	ctx := context.Background()
	input := map[string]any{"metadata": map[string]string{"name": "master"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}, "role": "master"}
	path := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), input)
	expectStatus(t, "nonadmin master", r, b, 403)
	u, err := ts.srv.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = ts.srv.store.SetUserAdmin(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), input)
	expectStatus(t, "approve master", r, b, 201)
	if b["role"] != "master" {
		t.Fatalf("role: %#v", b)
	}
	uid := b["metadata"].(map[string]any)["uid"].(string)
	hub.connected = true
	for range 4 {
		ts.srv.managed.reconcileOnce(ctx)
	}
	roster := ts.srv.managed.handleSwarmRequest(ctx, uid, 1, relay.SwarmRequest{Tool: "agents"})
	if !roster.OK || !strings.Contains(roster.Text, `"role":"master"`) {
		t.Fatalf("roster: %+v", roster)
	}
	pve.nextIDs = []int{106}
	pve.uncertain = true
	response := ts.srv.managed.handleSwarmRequest(ctx, uid, 1, relay.SwarmRequest{Tool: "add", To: "child"})
	if !response.OK || !strings.Contains(response.Text, "provisioning") {
		t.Fatalf("add: %+v", response)
	}
	child, err := ts.srv.store.LookupManagedActor(ctx, swarm, "child")
	if err != nil {
		t.Fatal(err)
	}
	master, err := ts.srv.store.GetManagedActor(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if child.PoolUID != master.PoolUID || child.Template != master.Template || child.Role != "worker" || child.DelegatorID != "" || child.ParentUID != master.UID {
		t.Fatal("child did not inherit approved binding")
	}
	creates := pve.creates
	response = ts.srv.managed.handleSwarmRequest(ctx, uid, 1, relay.SwarmRequest{Tool: "add", To: "child"})
	if !response.OK || pve.creates != creates {
		t.Fatalf("retry duplicated allocation: %+v", response)
	}
	for _, req := range []relay.SwarmRequest{{Tool: "add", To: "master"}, {Tool: "add", To: ""}, {Tool: "add", To: "../bad"}} {
		if got := ts.srv.managed.handleSwarmRequest(ctx, uid, 1, req); got.OK {
			t.Fatalf("invalid add accepted: %+v", req)
		}
	}
	if got := ts.srv.managed.handleSwarmRequest(ctx, uid, 2, relay.SwarmRequest{Tool: "add", To: "stale"}); got.OK {
		t.Fatal("stale master accepted")
	}
	if got := ts.srv.managed.handleSwarmRequest(ctx, child.UID, 1, relay.SwarmRequest{Tool: "add", To: "grandchild"}); got.OK {
		t.Fatal("worker accepted")
	}
	if err = ts.srv.store.SetUserAdmin(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := ts.srv.managed.handleSwarmRequest(ctx, uid, 1, relay.SwarmRequest{Tool: "add", To: "revoked"}); got.OK {
		t.Fatal("revoked delegator accepted")
	}
}

func TestManagedRoleDefaultAndNativeRejection(t *testing.T) {
	ts, alice, _, swarm, _, _ := managedSetup(t)
	path := "/api/swarms/" + swarm + "/actors"
	for _, role := range []any{"master", "worker", "invalid", nil} {
		r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "native"}, "role": role})
		if r.StatusCode != 400 {
			t.Fatalf("native role %v: %d %#v", role, r.StatusCode, b)
		}
	}
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "worker"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "worker", r, b, 201)
	if b["role"] != "worker" {
		t.Fatalf("default role: %#v", b)
	}
}
