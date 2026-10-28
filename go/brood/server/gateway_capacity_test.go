package server

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
)

// FEATURE: Swarm capacity is checked again inside the write transaction (#48).
// SPEC: Separate server/store connections cannot both reserve the final slot.
func TestGatewaySwarmCapacityMapsMissingProject(t *testing.T) {
	g, _, _ := swarmRaceFixture(t)
	err := g.SaveSwarm(app.Swarm{ID: "new", ProjectID: "missing", OwnerID: "alice", Name: "new", AtespaceName: "swarm-new", State: app.StateProvisioning})
	if !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("missing project error is not mapped: %v", err)
	}
}

func TestGatewaySwarmCapacityAcrossConnections(t *testing.T) {
	g, db, _ := swarmRaceFixture(t)
	ctx := context.Background()
	limit := int64(2)
	if err := db.SetProjectCapacity(ctx, "p", store.ProjectCapacity{MaxSwarms: &limit}); err != nil {
		t.Fatal(err)
	}
	var sequence int
	var name, path string
	if err := g.catalog.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	catalog, err := OpenCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	other := StoreGateway(second, catalog)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, gateway := range []*Gateway{g, other} {
		go func(i int, gateway *Gateway) {
			<-start
			id := []string{"first", "second"}[i]
			results <- gateway.SaveSwarm(app.Swarm{ID: id, ProjectID: "p", OwnerID: "alice", Name: id, AtespaceName: "swarm-" + id, State: app.StateProvisioning})
		}(i, gateway)
	}
	close(start)
	success, denied := 0, 0
	for range 2 {
		err := <-results
		var exceeded *app.CapacityExceededError
		if err == nil {
			success++
		} else if errors.As(err, &exceeded) && exceeded.Resource == "swarms" && exceeded.Used == 2 {
			denied++
		} else {
			t.Fatalf("unexpected save result: %v", err)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("last slot admitted %d writers, refused %d", success, denied)
	}
	rows, err := g.SwarmsByProject("p")
	if err != nil || len(rows) != 2 {
		t.Fatalf("persisted rows=%d, %v", len(rows), err)
	}
}
