package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/egress"
	"brood.dev/go/brood/store"
)

func serverNetworkWire(t *testing.T, host string) string {
	t.Helper()
	p, err := egress.Compile([]egress.Rule{{Protocol: egress.TLS, Hostnames: []string{host}, Ports: []int{8443}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestProjectSyncManagedNetworkHTTP(t *testing.T) {
	ts, alice, _, swarm, pve, _ := managedSetup(t)
	admin := newClient(t)
	ts.login(admin, "root", "root-password-1")
	sw, err := ts.srv.store.GetSwarm(context.Background(), swarm)
	if err != nil {
		t.Fatal(err)
	}
	wire := serverNetworkWire(t, "tool.example")
	request := map[string]any{"name": "proj", "repository": "", "managed_network": json.RawMessage(wire)}
	r, b, _ := ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), request)
	expectStatus(t, "sync network", r, b, 200)
	if b["changed"] != true {
		t.Fatal("network not reported as changed")
	}
	stored, err := ts.srv.store.ProjectManagedNetwork(context.Background(), sw.ProjectID)
	if err != nil || stored != wire {
		t.Fatalf("stored selected network: %v", err)
	}
	r, b, _ = ts.do(alice, "PUT", "/api/projects/sync", ts.csrf(alice), request)
	expectStatus(t, "idempotent member", r, b, 200)
	if b["changed"] != false {
		t.Fatal("idempotent member changed project")
	}
	request["managed_network"] = json.RawMessage(serverNetworkWire(t, "changed.example"))
	r, b, _ = ts.do(alice, "PUT", "/api/projects/sync", ts.csrf(alice), request)
	expectStatus(t, "member change refused", r, b, http.StatusForbidden)
	r, b, _ = ts.do(admin, "PUT", "/api/projects/sync", "", request)
	expectStatus(t, "network csrf", r, b, http.StatusForbidden)
	delete(request, "managed_network")
	r, b, _ = ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), request)
	expectStatus(t, "omitted network", r, b, 200)
	stored, _ = ts.srv.store.ProjectManagedNetwork(context.Background(), sw.ProjectID)
	if stored != wire {
		t.Fatal("omission cleared policy")
	}
	request["managed_network"] = nil
	r, b, _ = ts.do(admin, "PUT", "/api/projects/sync", ts.csrf(admin), request)
	expectStatus(t, "explicit network clear", r, b, 200)
	stored, _ = ts.srv.store.ProjectManagedNetwork(context.Background(), sw.ProjectID)
	if stored != "" {
		t.Fatal("explicit clear did not restore legacy default")
	}
	pve.mu.Lock()
	creates, boots := pve.creates, pve.bootstraps
	pve.mu.Unlock()
	if creates != 0 || boots != 0 {
		t.Fatal("project declaration mutated provider")
	}
}

func TestManagedNetworkCreateAndRestartUseReservedSnapshot(t *testing.T) {
	ts, alice, _, swarm, pve, hub := managedSetup(t)
	ctx := context.Background()
	sw, err := ts.srv.store.GetSwarm(ctx, swarm)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := ts.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	wire := serverNetworkWire(t, "original.example")
	_, err = ts.srv.store.SyncProject(ctx, admin.ID, sw.ProjectID, store.ProjectDeclaration{Name: "proj", ManagedNetwork: &wire}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("actor", nil))
	expectStatus(t, "create selected actor", r, b, 201)
	uid := b["metadata"].(map[string]any)["uid"].(string)
	actor, err := ts.srv.store.GetManagedActor(ctx, uid)
	if err != nil || actor.NetworkPolicy != wire {
		t.Fatalf("reservation lost policy: %v", err)
	}
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	for range 3 {
		ts.srv.managed.reconcileOnce(ctx)
	}
	changed := serverNetworkWire(t, "later.example")
	_, err = ts.srv.store.SyncProject(ctx, admin.ID, sw.ProjectID, store.ProjectDeclaration{Name: "proj", ManagedNetwork: &changed}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"stop", "resume"} {
		r, b, _ = ts.do(alice, "POST", path+"/actor", ts.csrf(alice), map[string]string{"action": action})
		expectStatus(t, action, r, b, 200)
		for range 2 {
			ts.srv.managed.reconcileOnce(ctx)
		}
	}
	pve.mu.Lock()
	envs := append([]proxmox.BootstrapEnv(nil), pve.envs...)
	pve.mu.Unlock()
	if len(envs) != 3 {
		t.Fatalf("expected create and two start snapshots, got %d", len(envs))
	}
	for _, env := range envs {
		if env["BROOD_GUEST_EGRESS_POLICY"] != wire {
			t.Fatal("create/start did not use original selected policy")
		}
		if _, legacy := env["BROOD_GUEST_ALLOWED_HOSTS"]; legacy {
			t.Fatal("selected policy mixed with legacy bootstrap")
		}
	}
	current, err := ts.srv.store.GetManagedActor(ctx, uid)
	if err != nil || current.NetworkPolicy != wire {
		t.Fatalf("restart changed snapshot: %v", err)
	}
}

func TestManagedSelectedNetworkOriginUsesPinnedTLSRule(t *testing.T) {
	ts, alice, _, swarm, pve, _ := managedSetup(t)
	ctx := context.Background()
	sw, err := ts.srv.store.GetSwarm(ctx, swarm)
	if err != nil {
		t.Fatal(err)
	}
	root, err := ts.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := egress.Compile([]egress.Rule{{Protocol: egress.TLS, Hostnames: []string{"selected.example"}, Ports: []int{443}}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatal(err)
	}
	wire := string(encoded)
	_, err = ts.srv.store.SyncProject(ctx, root.ID, sw.ProjectID, store.ProjectDeclaration{Name: "proj", ManagedNetwork: &wire}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/swarms/" + swarm + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("selected", "https://selected.example/team/repo"))
	expectStatus(t, "selected direct origin", r, b, 201)
	if b["originMode"] != "direct" {
		t.Fatal("selected TLS origin not classified as direct")
	}
	policy, err = egress.Compile([]egress.Rule{{Protocol: egress.HTTP, Hostnames: []string{"github.com"}, Ports: []int{443}}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = policy.Encode()
	changed := string(encoded)
	_, err = ts.srv.store.SyncProject(ctx, root.ID, sw.ProjectID, store.ProjectDeclaration{Name: "proj", ManagedNetwork: &changed}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	r, b, _ = ts.do(alice, "GET", path+"/selected", "", nil)
	expectStatus(t, "pinned direct origin", r, b, 200)
	if b["originMode"] != "direct" {
		t.Fatal("current default rewrote actor origin mode")
	}
	pve.mu.Lock()
	before := pve.creates
	pve.mu.Unlock()
	r, b, _ = ts.do(alice, "POST", path, ts.csrf(alice), managedOriginBody("denied", "https://github.com/team/repo"))
	expectStatus(t, "HTTP-only origin refused", r, b, 422)
	pve.mu.Lock()
	after := pve.creates
	pve.mu.Unlock()
	if after != before {
		t.Fatal("unapproved origin reached provider")
	}
}

type networkChangingPVE struct {
	proxmoxAPI
	change func()
}

func (p networkChangingPVE) NextID(ctx context.Context) (int, error) {
	id, err := p.proxmoxAPI.NextID(ctx)
	if err == nil {
		p.change()
	}
	return id, err
}

func TestManagedNetworkRechecksOriginAtReservation(t *testing.T) {
	ts, alice, _, swarm, pve, _ := managedSetup(t)
	ctx := context.Background()
	sw, err := ts.srv.store.GetSwarm(ctx, swarm)
	if err != nil {
		t.Fatal(err)
	}
	root, err := ts.srv.store.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	p, err := egress.Compile([]egress.Rule{{Protocol: egress.TLS, Hostnames: []string{"selected.example"}, Ports: []int{443}}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := p.Encode()
	wire := string(encoded)
	_, err = ts.srv.store.SyncProject(ctx, root.ID, sw.ProjectID, store.ProjectDeclaration{Name: "proj", ManagedNetwork: &wire}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	ts.srv.managed.newClient = func(proxmox.Config) (proxmoxAPI, error) {
		return networkChangingPVE{proxmoxAPI: pve, change: func() {
			changedPolicy, err := egress.Compile([]egress.Rule{{Protocol: egress.HTTP, Hostnames: []string{"selected.example"}, Ports: []int{443}}})
			if err != nil {
				t.Fatal(err)
			}
			data, _ := changedPolicy.Encode()
			changed := string(data)
			_, err = ts.srv.store.SyncProject(ctx, root.ID, sw.ProjectID, store.ProjectDeclaration{Name: "proj", ManagedNetwork: &changed}, func(context.Context) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
		}}, nil
	}
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), managedOriginBody("changed", "https://selected.example/team/repo"))
	expectStatus(t, "stale network origin", r, b, 422)
	actors, err := ts.srv.store.ListManagedActors(ctx, swarm)
	if err != nil {
		t.Fatal(err)
	}
	pve.mu.Lock()
	creates := pve.creates
	pve.mu.Unlock()
	if len(actors) != 0 || creates != 0 {
		t.Fatal("inconsistent origin/policy left an allocation or provider mutation")
	}
}

func TestProjectSyncManagedNetworkRejectsAmbiguity(t *testing.T) {
	ts, admin, _, _, _ := gitRepositorySetup(t)
	for _, field := range []string{
		`"managed_network":{"version":2,"rules":[]}`,
		`"managed_network":{"version":1,"rules":[],"private-token":true}`,
		`"managed_network":{"version":1,"rules":[],"rules":[]}`,
		`"Managed_network":{"version":1,"rules":[]}`,
		`"managed_network":"private-token"`,
	} {
		w := syncRaw(t, ts, admin, `{"name":"invalid-network","repository":"",`+field+`}`)
		if w.Code != 400 || strings.Contains(w.Body.String(), "private-token") {
			t.Fatalf("unsafe network refusal: status=%d", w.Code)
		}
	}
}
