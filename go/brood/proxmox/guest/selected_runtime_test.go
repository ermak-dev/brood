//go:build linux

package guest

import (
	"context"
	"testing"

	"brood.dev/go/brood/proxmox/egress"
)

func TestRuntimeStartsWithSelectedPolicy(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(map[bool]string{true: "offline", false: "selected"}[offline], func(t *testing.T) {
			var events []string
			c, _ := testConfig(&events)
			var rules []egress.Rule
			if !offline {
				rules = []egress.Rule{{Protocol: egress.HTTP, Hostnames: []string{"tool.example"}, Ports: []int{8080}}}
			}
			selected, err := egress.Compile(rules)
			if err != nil {
				t.Fatal(err)
			}
			c.Hosts = nil
			c.Network = selected
			services := c.Dependencies.Services
			called := false
			c.Dependencies.Services = func(ctx context.Context, p *Policy) (Services, error) {
				called = true
				if p.network == nil || p.network.Allows(egress.HTTP, "tool.example", 8080) == offline || p.network.Allows(egress.TLS, "tool.example", 443) {
					t.Error("supervisor broadened or dropped selected policy")
				}
				return services(ctx, p)
			}
			runtime, err := Start(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("network services did not receive selected policy")
			}
		})
	}
}

func TestRuntimeRejectsMixedLegacyAndSelectedPolicyBeforeEffects(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	c.Network, _ = egress.Compile(nil)
	if runtime, err := Start(context.Background(), c); err == nil || runtime != nil {
		if runtime != nil {
			runtime.Close()
		}
		t.Fatal("mixed policies accepted")
	}
	if len(events) != 0 {
		t.Fatalf("ambiguous policy caused effects: %v", events)
	}
}
