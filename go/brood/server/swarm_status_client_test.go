package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"brood.dev/go/brood/clientapi"
)

func TestSwarmStatusClientExistingEndpoint(t *testing.T) {
	f := startAPISetup(t)
	client, err := clientapi.New(f.ts.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse(f.ts.http.URL)
	stored := clientapi.StoredSession{}
	for _, cookie := range f.alice.Jar.Cookies(origin) {
		if cookie.Name == "session" {
			stored.Session = cookie.Value
		}
		if cookie.Name == "csrf" {
			stored.CSRF = cookie.Value
		}
	}
	client.Restore(stored)
	// Непривязанный рой намеренно не имеет execution view.
	if _, err := client.SwarmStatus(context.Background(), f.swarm); err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("unpinned swarm: %v", err)
	}
	r, b := f.start(f.alice, map[string]any{"definition": "approve", "goal": "fixture goal", "commit": startAPICommit})
	expectStatus(t, "fixture start", r, b, http.StatusOK)
	beforeExecution, beforeLife := f.executionState(t), f.lifeState(t)
	view, err := client.SwarmStatus(context.Background(), f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != f.swarm || view.SwarmID != f.swarm || view.Status != "waiting_human" || view.Goal != "fixture goal" || view.Definition == nil || view.Definition.Name != "approve" || len(view.Nodes) != 1 {
		t.Fatalf("actual contract: %+v", view)
	}
	node := view.Nodes[0]
	if node.Sequence != 1 || node.Pass != 1 || node.Goal != "fixture goal" || node.SourceCommit != startAPICommit || node.Status != "active" || view.CurrentNode != node.ID {
		t.Fatalf("actual visit: %+v", node)
	}
	if view.Gate == nil || view.Gate.Sequence != node.Sequence {
		t.Fatalf("actual gate: %+v", view.Gate)
	}
	if f.executionState(t) != beforeExecution || f.lifeState(t) != beforeLife {
		t.Fatal("status read changed fixture state")
	}
}
