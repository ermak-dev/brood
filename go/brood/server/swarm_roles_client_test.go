package server

// FEATURE: bro-клиент привязки ролей против настоящего сервера (#235).
// SPEC: clientapi читает представление ролей, находит managed-актора с его
// инкарнацией, привязывает и снимает роль; каждая фиксированная причина
// отказа сервера доходит до вызывающего клиента, после привязки запуск роя
// проходит проверку ролей.
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"brood.dev/go/brood/clientapi"
)

func roleClient(t *testing.T, f startAPIFixture, username string) *clientapi.Client {
	t.Helper()
	client, err := clientapi.New(f.ts.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Login(context.Background(), username, username+"-password-1"); err != nil {
		t.Fatal(err)
	}
	return client
}

// expectReason — отказ клиента несёт ровно фиксированную причину сервера.
func expectReason(t *testing.T, what string, err error, reason string) {
	t.Helper()
	if err == nil || !strings.HasSuffix(err.Error(), ": "+reason) {
		t.Fatalf("%s: %v, want reason %q", what, err, reason)
	}
}

func TestSwarmRolesThroughClient(t *testing.T) {
	ctx := context.Background()
	f := startAPISetup(t)
	alice, bob := roleClient(t, f, "alice"), roleClient(t, f, "bob")

	_, err := bob.SwarmRoles(ctx, f.swarm, "")
	expectReason(t, "outsider roles", err, "swarm not found")
	r, b, _ := f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/members", f.ts.csrf(f.alice), map[string]string{"username": "bob", "role": "viewer"})
	expectStatus(t, "add viewer", r, b, http.StatusNoContent)
	_, err = bob.BindSwarmRole(ctx, f.swarm, "review", f.master.UID, 0)
	expectReason(t, "viewer bind", err, "swarm management denied")

	actors, err := alice.SwarmActors(ctx, f.swarm)
	if err != nil || len(actors) != 1 || actors[0].UID != f.master.UID || actors[0].Name != "master" || actors[0].Incarnation != f.master.Incarnation {
		t.Fatalf("swarm actors: %+v %v", actors, err)
	}
	view, err := alice.SwarmRoles(ctx, f.swarm, "review")
	if err != nil || view.Graph == nil || view.Graph.Name != "review" || len(view.Roles) != 2 || view.Roles[1].Role != "review" || view.Roles[1].State != "unbound" {
		t.Fatalf("catalog roles: %+v %v", view, err)
	}
	for _, tc := range []struct {
		what, definition, reason string
	}{
		{"missing definition", "absent", "workflow definition is not in the project catalog"},
		{"non-executable definition", "retrying", "workflow graph is not executable"},
	} {
		_, err := alice.SwarmRoles(ctx, f.swarm, tc.definition)
		expectReason(t, tc.what, err, tc.reason)
	}
	for _, tc := range []struct {
		what, role, actor string
		incarnation       int64
		reason            string
	}{
		{"invalid role", "Review", f.master.UID, 0, "swarm role must be a lowercase DNS label"},
		{"unknown actor", "review", "absent", 0, "actor is not an actor of this swarm"},
		{"another incarnation", "review", f.master.UID, f.master.Incarnation + 1, "actor incarnation changed; list the swarm actors and bind again"},
	} {
		_, err := alice.BindSwarmRole(ctx, f.swarm, tc.role, tc.actor, tc.incarnation)
		expectReason(t, tc.what, err, tc.reason)
	}
	_, err = alice.UnbindSwarmRole(ctx, f.swarm, "review")
	expectReason(t, "unbind unbound role", err, "swarm role is not bound")

	setManagedPhase(t, f, "stopped")
	_, err = alice.BindSwarmRole(ctx, f.swarm, "review", f.master.UID, actors[0].Incarnation)
	expectReason(t, "stopped actor", err, "actor is not live: it must be provisioning or running and meant to run")
	setManagedPhase(t, f, "running")

	view, err = alice.BindSwarmRole(ctx, f.swarm, "review", actors[0].UID, actors[0].Incarnation)
	if err != nil || len(view.Roles) != 2 || view.Roles[1].Role != "review" || view.Roles[1].State != "ready" || view.Roles[1].ActorName != "master" {
		t.Fatalf("bind review: %+v %v", view, err)
	}
	r, b = f.start(f.alice, map[string]any{"goal": startAPIGoal, "definition": "review", "commit": startAPICommit})
	expectStatus(t, "start after bind", r, b, http.StatusOK)
	_, err = alice.UnbindSwarmRole(ctx, f.swarm, "review")
	expectReason(t, "unbind while running", err, "swarm execution is running or waiting; stop the swarm before changing role bindings")
	_, err = alice.SwarmRoles(ctx, f.swarm, "release")
	expectReason(t, "another definition of a pinned swarm", err, "swarm graph is pinned to another workflow definition")
	r, b = f.stop(f.alice)
	expectStatus(t, "stop", r, b, http.StatusOK)

	if err := f.ts.srv.store.SetSwarmState(ctx, f.swarm, "error"); err != nil {
		t.Fatal(err)
	}
	_, err = alice.UnbindSwarmRole(ctx, f.swarm, "review")
	expectReason(t, "swarm not active", err, "swarm is not active")
}
