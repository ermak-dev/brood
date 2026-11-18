//go:build linux

package main

import (
	"context"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/egress"
	"brood.dev/go/brood/proxmox/relay"
)

func selectedEnv(t *testing.T, raw string) []string {
	t.Helper()
	result := []string{}
	for _, entry := range validEnv(t) {
		if !strings.HasPrefix(entry, "BROOD_GUEST_ALLOWED_HOSTS=") {
			result = append(result, entry)
		}
	}
	return append(result, "BROOD_GUEST_EGRESS_POLICY="+raw, "BROOD_GUEST_STAGE=runtime")
}

func TestSelectedBootstrapConfigPreservesPolicy(t *testing.T) {
	raw := `{"version":1,"rules":[{"protocol":"tls","hostnames":["tool.example"],"ports":[8443]}]}`
	c, err := parseConfig(selectedEnv(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	if c.network == nil || !c.network.Allows(egress.TLS, "tool.example", 8443) || c.network.Allows(egress.HTTP, "tool.example", 8443) || c.network.Allows(egress.TLS, "tool.example", 443) {
		t.Fatal("bootstrap lost selected protocol/port")
	}
	found := false
	for _, entry := range c.bootstrapEnv {
		if entry == "BROOD_GUEST_EGRESS_POLICY="+raw {
			found = true
		}
		if strings.HasPrefix(entry, "BROOD_GUEST_ALLOWED_HOSTS=") {
			t.Fatal("selected policy became a legacy array")
		}
	}
	if !found {
		t.Fatal("selected policy lost during runtime reexec")
	}
	offline, err := parseConfig(selectedEnv(t, `{"version":1,"rules":[]}`))
	if err != nil || offline.network == nil || offline.network.AllowsDNS("tool.example") {
		t.Fatalf("offline bootstrap: %v", err)
	}
	legacy, err := parseConfig(validEnv(t))
	if err != nil || legacy.network != nil {
		t.Fatalf("legacy configuration changed: %v", err)
	}
}

func TestSelectedBootstrapRejectsAmbiguityBeforeEffects(t *testing.T) {
	for _, env := range [][]string{
		append(validEnv(t), `BROOD_GUEST_EGRESS_POLICY={"version":1,"rules":[]}`),
		selectedEnv(t, `{"version":1,"rules":[],"private-token":true}`),
		selectedEnv(t, `{"version":2,"rules":[]}`),
		selectedEnv(t, `{"version":1,"rules":null}`),
		selectedEnv(t, ``),
	} {
		err := dispatch(context.Background(), nil, func() []string { return env }, dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) { t.Fatal("bad policy reached network setup"); return nil, nil }})
		if err == nil || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("unsafe policy failure: %v", err)
		}
	}
}

func TestSelectedBootstrapCannotFallbackToLegacyStarter(t *testing.T) {
	err := dispatch(context.Background(), nil, func() []string { return selectedEnv(t, `{"version":1,"rules":[]}`) }, dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) {
		t.Fatal("missing selected starter reached side effects")
		return nil, nil
	}, start: func(context.Context, []string) (actorRuntime, error) {
		t.Fatal("selected policy fell back to legacy starter")
		return nil, nil
	}})
	if err == nil {
		t.Fatal("missing selected policy support accepted")
	}
}

func TestSelectedBootstrapRuntimeUsesPolicyStarter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, runtime := newFake(), newFake()
	called := false
	d := dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) { return network, nil }, start: func(context.Context, []string) (actorRuntime, error) {
		t.Fatal("legacy policy starter used")
		return nil, nil
	}, startPolicy: func(_ context.Context, p *egress.Policy) (actorRuntime, error) {
		called = true
		if p == nil || !p.Allows(egress.TLS, "tool.example", 8443) || p.Allows(egress.HTTP, "tool.example", 8443) {
			t.Error("wrong runtime policy")
		}
		return runtime, nil
	}, relay: func(context.Context, relay.ClientConfig) error { cancel(); return context.Canceled }}
	err := dispatch(ctx, nil, func() []string {
		return selectedEnv(t, `{"version":1,"rules":[{"protocol":"tls","hostnames":["tool.example"],"ports":[8443]}]}`)
	}, d)
	if err != nil || !called || !runtime.closed || !network.closed {
		t.Fatalf("selected runtime: error=%v called=%v runtimeClosed=%v networkClosed=%v", err, called, runtime.closed, network.closed)
	}
}
