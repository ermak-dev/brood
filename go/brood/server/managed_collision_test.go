package server

import (
	"context"
	"net/http"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// BUG: Managed terminal tickets accepted a name rejected as ambiguous by CRUD.
// SPEC: A native collision revokes tickets, resolution and existing dial targets.
func TestManagedTerminalRejectsNativeNameCollision(t *testing.T) {
	ts, alice, _, id, _, hub := managedSetup(t)
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+id+"/actors", ts.csrf(alice), map[string]any{"metadata": map[string]string{"name": "collision"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}})
	expectStatus(t, "create", r, b, 201)
	hub.connected = true
	for range 3 {
		ts.srv.managed.reconcileOnce(context.Background())
	}
	sw, _, _ := ts.srv.gateway.SwarmByID(id)
	resolver := &managedResolver{server: ts.srv, native: NewSubstrateResolver(ts.srv.ateapi, "")}
	target, err := resolver.ActorUpstream(sw.AtespaceName, "collision")
	if err != nil {
		t.Fatal(err)
	}
	client := *alice
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Get(ts.http.URL + "/api/swarms/" + id + "/proxy/actors/collision/")
	if err != nil {
		t.Fatal(err)
	}
	location := response.Header.Get("Location")
	response.Body.Close()
	if response.StatusCode != 303 || location == "" {
		t.Fatal("initial ticket missing")
	}
	ts.srv.views.mu.Lock()
	var ticket actorView
	for _, v := range ts.srv.views.tickets {
		if v.actor == "collision" {
			ticket = v
		}
	}
	ts.srv.views.mu.Unlock()
	ts.fake.mu.Lock()
	ts.fake.actors[sw.AtespaceName+"/collision"] = &ateapi.Actor{Metadata: fakeMeta("collision", sw.AtespaceName), ActorTemplate: &ateapi.ObjectRef{Name: "native", Atespace: sw.AtespaceName}}
	ts.fake.mu.Unlock()
	r, b, _ = ts.do(alice, "GET", "/api/swarms/"+id+"/actors/collision", "", nil)
	expectStatus(t, "ambiguous actor", r, b, 409)
	if _, err = resolver.ActorUpstream(sw.AtespaceName, "collision"); err == nil {
		t.Error("ambiguous managed name resolved")
	}
	if ts.srv.validActorView(ticket) {
		t.Error("existing ticket remained authorized")
	}
	if ts.srv.managed.validTarget(target) {
		t.Error("existing target remained authorized")
	}
	if conn, e := ts.srv.managed.dial(context.Background(), "tcp", target.Host); e == nil {
		conn.Close()
		t.Error("ambiguous target dialed")
	}
	response, err = client.Get(ts.http.URL + "/api/swarms/" + id + "/proxy/actors/collision/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 || response.Header.Get("Location") != "" {
		t.Error("new ambiguous ticket issued")
	}
	response, err = http.Get(ts.http.URL + location)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Error("existing ambiguous ticket served")
	}
}
