package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/egress"
)

func TestManagedNetworkRejectsUntransportablePolicyBeforeStorage(t *testing.T) {
	s, ctx := networkFixture(t)
	previous := networkWire(t, "existing.example")
	syncNetwork(t, s, ctx, &previous)
	hosts := make([]string, 128)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%03d.%s.example.com", i, strings.Repeat("a", 63))
	}
	raw, err := json.Marshal(struct {
		Version int           `json:"version"`
		Rules   []egress.Rule `json:"rules"`
	}{1, []egress.Rule{{Protocol: egress.TLS, Hostnames: hosts, Ports: []int{443}}}})
	mustNil(t, err)
	if len(raw) <= 8192 || len(raw) > 128<<10 {
		t.Fatalf("invalid oversized fixture: %d bytes", len(raw))
	}
	before := dumpTables(t, s, "projects", "project_managed_networks", "managed_actors", "managed_actor_networks")
	wire := string(raw)
	_, err = s.SyncProject(ctx, "network-admin", "one", ProjectDeclaration{Name: "one", ManagedNetwork: &wire}, func(context.Context) bool { return true })
	if err == nil {
		t.Errorf("stored a %d-byte policy that cannot fit the bootstrap environment", len(raw))
	}
	if after := dumpTables(t, s, "projects", "project_managed_networks", "managed_actors", "managed_actor_networks"); after != before {
		t.Error("untransportable policy changed persistent project or actor state")
	}
	retained, err := s.ProjectManagedNetwork(ctx, "one")
	mustNil(t, err)
	if retained != previous {
		t.Error("untransportable policy replaced the previously valid project default")
	}
}
