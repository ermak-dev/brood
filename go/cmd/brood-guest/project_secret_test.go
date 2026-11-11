//go:build linux

package main

// FEATURE: Refresh project secrets before cold application launch.
// SPEC: Root fetch runs after network and before app; failure prevents app launch;
// fixed app dispatch never reads bootstrap env; refresh is connection-bound.
// IMPLEMENTATION: Injectable root refresh before runtime startup and fixed app
// dispatch before bootstrap environment access; relay factories capture identity.
// RESULT: IMPLEMENTED — cold fetch failures prevent app startup and close DHCP.
// STATUS: DONE

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/relay"
)

func TestProjectSecretColdStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := newFake()
	var events []string
	d := dependencies{euid: func() int { return 0 }, artifacts: func(context.Context, relay.ClientConfig, []string, guest.ArtifactInstaller) error {
		return nil
	}, reexec: func([]string) error { return nil }, network: func(context.Context) (service, error) { events = append(events, "network"); return network, nil }, secrets: func(context.Context, relay.ClientConfig) error {
		events = append(events, "secrets")
		return errors.New("opaque")
	}, start: func(context.Context, []string) (actorRuntime, error) {
		t.Fatal("app launched without secrets")
		return nil, nil
	}}
	// Стадия runtime: секреты проверяются после exec от установленных
	// артефактов (#155); bootstrap-стадию качает отдельный тест.
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	err := dispatch(ctx, nil, func() []string { return append(validEnv(t), "BROOD_GUEST_STAGE=runtime") }, d)
	if err == nil || strings.Contains(err.Error(), "opaque") || !reflect.DeepEqual(events, []string{"network", "secrets"}) || !network.closed {
		t.Fatal("cold secret failure not closed", events, err)
	}
}
func TestProjectSecretAppDispatch(t *testing.T) {
	called := false
	d := dependencies{app: func() error { called = true; return nil }}
	err := dispatch(context.Background(), []string{"--brood-guest-app"}, func() []string { t.Fatal("app read bootstrap env"); return nil }, d)
	if err != nil || !called {
		t.Fatal("missing app dispatch", err)
	}
}
