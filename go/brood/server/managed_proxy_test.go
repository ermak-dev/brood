package server

// FEATURE: Managed terminal tickets and machine-authenticated relay route.
// SPEC: Tickets bind managed UID and generation; guest endpoints bypass cookie
// CSRF but never accept cookies as machine authority. Native UID checks remain.
// IMPLEMENTATION: Per-process verified adoption gates terminal routing; successful
// refresh keeps live terminals usable and ownership failures revoke readiness.
// RESULT: IMPLEMENTED — persisted running state never bypasses verified adoption.
// STATUS: DONE

import (
	"context"
	"net/http"
	"testing"

	"brood.dev/go/brood/proxmox"
)

// FEATURE: Restart adoption verifies current owned PVE configuration.
// SPEC: Durable running state plus an authenticated relay is not sufficient;
// this server must inspect ownership and task/terminal readiness before proxying.
func TestManagedRestartRequiresVerifiedAdoption(t *testing.T) {
	ts, alice, _, id, pve, hub := managedSetup(t)
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+id+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "adopt"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	hub.connected = true
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	sw, _, _ := ts.srv.gateway.SwarmByID(id)
	target, err := (&managedResolver{server: ts.srv}).ActorUpstream(sw.AtespaceName, "adopt")
	if err != nil {
		t.Fatal(err)
	}
	restarted := &managedProvider{server: ts.srv, registry: ts.srv.managed.registry, managedRuntime: managedRuntime{hub: hub, clients: map[string]proxmoxAPI{}, newClient: func(proxmox.Config) (proxmoxAPI, error) { return pve, nil }}}
	if restarted.validTarget(target) {
		t.Fatal("persisted running actor exposed before restart ownership inspection")
	}
	// BUG: Истекший лог уже завершённого UPID ломает adoption живого CT.
	// HYPOTHESIS: Stage=done игнорируется перед повторным WaitTask.
	// SOLUTION: Завершённый UPID не опрашивается; ownership/relay/ttyd проверяются заново.
	// RESULT: FIXED — живой owned CT принимается без повторных записей и чтения старого task.
	// STATUS: FIXED
	a := mustManagedActor(t, ts, id)
	pve.taskErrors = map[string]error{a.UPID: proxmox.ErrNotFound}
	waits := len(pve.waited)
	restarted.reconcileOnce(context.Background())
	if len(pve.waited) != waits {
		t.Error("completed task polled again during restart adoption")
	}
	if !restarted.validTarget(target) {
		t.Fatal("valid owned actor not adopted after task, relay and HTTP verification")
	}
	pve.inspectStarted, pve.inspectRelease = make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() { restarted.reconcileOnce(context.Background()); close(done) }()
	<-pve.inspectStarted
	validDuringRefresh := restarted.validTarget(target)
	close(pve.inspectRelease)
	<-done
	pve.inspectStarted, pve.inspectRelease = nil, nil
	if !validDuringRefresh {
		t.Fatal("routine successful reconciliation transiently revoked a live terminal")
	}
	pve.inspectError = proxmox.ErrOwnership
	restarted.reconcileOnce(context.Background())
	if restarted.validTarget(target) {
		t.Fatal("changed ownership remained routable")
	}
	if pve.creates != 1 || pve.starts != 1 {
		t.Fatal("adoption duplicated provider writes")
	}
}

func TestManagedProxyResolutionAndMachineRoute(t *testing.T) {
	ts, alice, _, id, _, hub := managedSetup(t)
	path := "/api/swarms/" + id + "/actors"
	r, b, _ := ts.do(alice, "POST", path, ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "terminal"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	uid := b["metadata"].(map[string]any)["uid"].(string)
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	sw, _, _ := ts.srv.gateway.SwarmByID(id)
	target, err := (&managedResolver{server: ts.srv, native: NewSubstrateResolver(ts.srv.ateapi, "")}).ActorUpstream(sw.AtespaceName, "terminal")
	if err != nil {
		t.Fatal(err)
	}
	if !ts.srv.managed.validTarget(target) {
		t.Fatal("current managed target not valid")
	}
	conn, err := ts.srv.managed.dial(context.Background(), "tcp", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	forged := *target
	forged.Host = "127.0.0.1:8080"
	if _, err := ts.srv.managed.dial(context.Background(), "tcp", forged.Host); err == nil {
		t.Fatal("relay forwarded arbitrary network target")
	}
	request, _ := http.NewRequest("GET", ts.http.URL+"/api/swarms/"+id+"/proxy/actors/terminal/?mode=rw", nil)
	// Avoid following the redirect; the location itself is the scoped ticket.
	client := *alice
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	location := response.Header.Get("Location")
	response.Body.Close()
	if response.StatusCode != 303 {
		t.Fatalf("ticket response %d", response.StatusCode)
	}
	a, _ := ts.srv.store.GetManagedActor(context.Background(), uid)
	ts.srv.views.mu.Lock()
	var ticket actorView
	for _, v := range ts.srv.views.tickets {
		if v.actor == "terminal" {
			ticket = v
		}
	}
	ts.srv.views.mu.Unlock()
	if ticket.managedUID != uid || ticket.generation != a.Incarnation || !ts.srv.validActorView(ticket) {
		t.Fatal("ticket not bound to current managed incarnation")
	}
	// A reset changes the generation before any replacement endpoint can be used.
	_, err = ts.srv.store.BeginManagedActorOperation(context.Background(), a.UID, a.Revision, "reset-test", "reset")
	if err != nil {
		t.Fatal(err)
	}
	if ts.srv.validActorView(ticket) || ts.srv.managed.validTarget(target) {
		t.Fatal("old generation survived revocation")
	}
	resp, err := http.Get(ts.http.URL + location)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("stale ticket status %d", resp.StatusCode)
	}
	machine := ts.http.URL + "/api/proxmox/guest/" + uid + "/1"
	r, b, _ = ts.do(alice, "GET", machine[len(ts.http.URL):], "", nil)
	expectStatus(t, "cookie is not machine auth", r, b, 401)
}
