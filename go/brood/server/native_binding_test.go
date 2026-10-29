package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/app"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// BUG: Старый ACL роя открывал новый native namespace с тем же именем (#79).
// HYPOTHESIS: Чтения, suspend/delete и actor-view не сверяют живой namespace UID.
// RESULT: FIXED — namespace fencing и subject UID precondition проверены через HTTP/gRPC.
func TestActorViewRevalidationRejectsReplacedNamespace(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	_, owner, _, swarm := actorsSetup(t, ts.testServer)
	owner.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r, body, _ := ts.do(owner, "GET", "/api/swarms/"+swarm+"/proxy/actors/probe/", "", nil)
	expectStatus(t, "mint original ticket", r, body, 303)
	ts.srv.views.mu.Lock()
	var view actorView
	for _, ticket := range ts.srv.views.tickets {
		view = ticket
	}
	ts.srv.views.mu.Unlock()
	if !ts.srv.validActorView(view) {
		t.Fatal("original binding was not valid")
	}
	if allowed, err := ts.srv.canUseNative(view.userID, app.ActionManageSwarm, app.SwarmRes(swarm)); err != nil || !allowed {
		t.Fatalf("original direct proxy binding denied: %v", err)
	}
	bound, found, err := ts.srv.gateway.SwarmByID(swarm)
	if err != nil || !found {
		t.Fatalf("stored binding unavailable: %v", err)
	}
	if view.atespaceUID == "" || view.atespaceUID != bound.AtespaceUID {
		t.Fatal("ticket did not capture its original native UID")
	}
	stale := view
	stale.atespaceUID = "other-native-identity"
	if ts.srv.validActorView(stale) {
		t.Fatal("ticket from a different binding was accepted")
	}
	fake.mu.Lock()
	fake.atespaces[view.atespace].Metadata.Uid = "70d3e1a3-01f8-4aa5-86bc-9ac623e5fd40"
	fake.mu.Unlock()
	if ts.srv.validActorView(view) {
		t.Fatal("old ticket survived native namespace replacement")
	}
	if allowed, err := ts.srv.canUseNative(view.userID, app.ActionManageSwarm, app.SwarmRes(swarm)); allowed || err == nil {
		t.Fatal("direct proxy accepted replacement native namespace")
	}
	if allowed, err := ts.srv.app.Can(view.userID, app.ActionManageSwarm, app.SwarmRes(swarm)); err != nil || !allowed {
		t.Fatal("namespace test was confounded by loss of local authorization")
	}
}

func TestNativeReadsRejectReplacementDuringRPC(t *testing.T) {
	for _, request := range []struct{ method, path string }{
		{"GET", "/actors"},
		{"GET", "/actors/foreign"},
		{"GET", "/templates"},
		{"GET", "/templates/foreign-template"},
		{"DELETE", "/actors/foreign"},
		{"DELETE", "/templates/foreign-template"},
	} {
		t.Run(request.method+request.path, func(t *testing.T) {
			fake := newFakeAteapi()
			ts := newSubstrateServer(t, fake)
			_, owner, _, swarm := actorsSetup(t, ts.testServer)
			bound, _, err := ts.srv.gateway.SwarmByID(swarm)
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			fake.beforeResourceRead = func() {
				fake.beforeResourceRead = nil
				fake.atespaces[bound.AtespaceName].Metadata.Uid = "70d3e1a3-01f8-4aa5-86bc-9ac623e5fd40"
				fake.actors[bound.AtespaceName+"/foreign"] = &ateapi.Actor{Metadata: fakeMeta("foreign", bound.AtespaceName)}
				fake.templates[bound.AtespaceName+"/foreign-template"] = &ateapi.ActorTemplate{Metadata: fakeMeta("foreign-template", bound.AtespaceName)}
			}
			fake.mu.Unlock()
			r, body, raw := ts.do(owner, request.method, "/api/swarms/"+swarm+request.path, ts.csrf(owner), nil)
			expectStatus(t, "in-flight replacement denied", r, body, 409)
			if strings.Contains(raw, "foreign-template") || strings.Contains(raw, "uid-foreign") {
				t.Fatal("in-flight replacement data was returned")
			}
		})
	}
}

func TestNativeInventoryRejectsReplacementDuringRPC(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	_, _, _, swarm := actorsSetup(t, ts.testServer)
	bound, found, err := ts.srv.gateway.SwarmByID(swarm)
	if err != nil || !found {
		t.Fatalf("stored binding unavailable: %v", err)
	}
	fake.mu.Lock()
	fake.beforeResourceRead = func() {
		fake.beforeResourceRead = nil
		fake.atespaces[bound.AtespaceName].Metadata.Uid = "replacement-inventory-namespace"
	}
	fake.mu.Unlock()
	if _, err := nativeShardReservations(context.Background(), ts.srv.ateapi, []app.Swarm{bound}); err == nil {
		t.Fatal("inventory accepted data from a replaced namespace")
	}
}

func TestNativeDeletesRejectSubjectReplacement(t *testing.T) {
	for _, kind := range []string{"actors", "templates"} {
		t.Run(kind, func(t *testing.T) {
			fake := newFakeAteapi()
			ts := newSubstrateServer(t, fake)
			_, owner, _, swarm := actorsSetup(t, ts.testServer)
			bound, _, err := ts.srv.gateway.SwarmByID(swarm)
			if err != nil {
				t.Fatal(err)
			}
			key := bound.AtespaceName + "/subject"
			fake.mu.Lock()
			if kind == "actors" {
				fake.actors[key] = &ateapi.Actor{Metadata: fakeMeta("subject", bound.AtespaceName)}
			} else {
				fake.templates[key] = &ateapi.ActorTemplate{Metadata: fakeMeta("subject", bound.AtespaceName)}
			}
			fake.beforeResourceDelete = func() {
				fake.beforeResourceDelete = nil
				metadata := fakeMeta("subject", bound.AtespaceName)
				metadata.Uid = "replacement-subject"
				if kind == "actors" {
					fake.actors[key] = &ateapi.Actor{Metadata: metadata}
				} else {
					fake.templates[key] = &ateapi.ActorTemplate{Metadata: metadata}
				}
			}
			fake.mu.Unlock()
			r, body, _ := ts.do(owner, "DELETE", "/api/swarms/"+swarm+"/"+kind+"/subject", ts.csrf(owner), nil)
			expectStatus(t, "replacement deletion denied", r, body, 409)
			fake.mu.Lock()
			defer fake.mu.Unlock()
			var uid string
			if kind == "actors" {
				uid = fake.actors[key].GetMetadata().GetUid()
			} else {
				uid = fake.templates[key].GetMetadata().GetUid()
			}
			if uid != "replacement-subject" {
				t.Fatal("replacement subject was removed")
			}
		})
	}
}

func TestSwarmNativeRoutesRejectReplacedNamespace(t *testing.T) {
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/actors", nil},
		{"GET", "/actors/foreign", nil},
		{"GET", "/templates", nil},
		{"GET", "/templates/foreign-template", nil},
		{"POST", "/actors/foreign", map[string]string{"action": "suspend"}},
		{"DELETE", "/actors/foreign", nil},
		{"GET", "/proxy/actors/foreign/", nil},
		{"GET", "/policy", nil},
		{"POST", "/policy", map[string]any{}},
		{"PUT", "/policy", map[string]any{}},
		{"DELETE", "/policy", nil},
	} {
		t.Run(request.method+request.path, func(t *testing.T) {
			fake := newFakeAteapi()
			ts := newSubstrateServer(t, fake)
			admin, owner, _, swarm := actorsSetup(t, ts.testServer)
			if request.path == "/policy" {
				owner = admin
			}
			owner.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			row, err := ts.srv.store.GetSwarm(context.Background(), swarm)
			if err != nil {
				t.Fatal(err)
			}
			base := "/api/swarms/" + swarm
			r, body, _ := ts.do(owner, "GET", base+"/actors", "", nil)
			expectStatus(t, "original binding", r, body, 200)
			fake.mu.Lock()
			fake.atespaces[row.AtespaceName].Metadata.Uid = "70d3e1a3-01f8-4aa5-86bc-9ac623e5fd40"
			fake.actors[row.AtespaceName+"/foreign"] = &ateapi.Actor{
				Metadata: &ateapi.ResourceMetadata{Name: "foreign", Atespace: row.AtespaceName,
					Uid: "1b9a21b1-d2d7-4b54-9d72-130f4d1373b5"},
				ActorTemplate: &ateapi.ObjectRef{Name: "foreign-template", Atespace: row.AtespaceName},
				Status:        &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_RUNNING},
			}
			fake.templates[row.AtespaceName+"/foreign-template"] = &ateapi.ActorTemplate{
				Metadata: &ateapi.ResourceMetadata{Name: "foreign-template", Atespace: row.AtespaceName,
					Uid: "d5b46edb-994d-4541-a814-6f48b14ae4dc"},
			}
			fake.mu.Unlock()
			r, body, raw := ts.do(owner, request.method, base+request.path, ts.csrf(owner), request.body)
			expectStatus(t, "replacement namespace denied", r, body, 409)
			if strings.Contains(raw, "foreign-template") || strings.Contains(raw, "70d3e1a3") {
				t.Fatal("replacement namespace content leaked")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			actor := fake.actors[row.AtespaceName+"/foreign"]
			if actor == nil || actor.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
				t.Fatal("foreign replacement actor was modified")
			}
		})
	}
}
