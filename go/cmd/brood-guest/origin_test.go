//go:build linux

package main

// FEATURE: Workspace origin fetched at boot (#147).
// SPEC: The bootstrap parser keeps the closed name list of main, so
// BROOD_GUEST_ORIGIN and BROOD_GUEST_ORIGIN_MODE are unknown variables. The
// root supervisor fetches the origin once from the server, after the project
// secrets cold fetch and before the broker, the gateways and the actor, with
// the bootstrap identity and the approved hosts. The Git gateway starts only
// for a fetched gateway mode, before the actor; a failed fetch or gateway stops
// the guest before the actor without leaking the cause.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/relay"
)

func TestConfigRefusesWorkspaceOriginVariables(t *testing.T) {
	env := validEnv(t)
	for _, extra := range [][]string{
		{"BROOD_GUEST_ORIGIN=https://git.example.org/group/repo.git", "BROOD_GUEST_ORIGIN_MODE=gateway"},
		{"BROOD_GUEST_ORIGIN=https://github.com/owner/repo"},
		{"BROOD_GUEST_ORIGIN_MODE=direct"},
		{"BROOD_GUEST_ORIGIN=", "BROOD_GUEST_ORIGIN_MODE="},
	} {
		_, err := parseConfig(append(append([]string{}, env...), extra...))
		if err == nil || err.Error() != "unknown guest configuration variable" {
			t.Fatalf("%v: %v", extra, err)
		}
	}
}

var fetchedOrigins = map[string]guest.WorkspaceOrigin{
	"gateway": {Origin: "https://git.example.org/repo.git", Mode: "gateway"},
	"direct":  {Origin: "https://github.com/owner/repo", Mode: "direct"},
	"":        {},
}

func TestGitGatewayOnlyInGatewayModeBeforeActor(t *testing.T) {
	for _, mode := range []string{"gateway", "direct", ""} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			network, runtime, model, git := newFake(), newFake(), newFake(), newFake()
			var events []string
			d := dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) { return network, nil },
				secrets: func(context.Context, relay.ClientConfig) error {
					events = append(events, "secrets")
					return nil
				},
				origin: func(_ context.Context, c relay.ClientConfig, hosts []string) (guest.WorkspaceOrigin, error) {
					if c.Token != "private-token" || c.ActorUID != "actor-1" || c.Generation != 1 || c.URL != "wss://brood.example" || !reflect.DeepEqual(hosts, []string{"github.com"}) {
						t.Error("origin fetch without the bootstrap identity or approved hosts")
					}
					events = append(events, "origin")
					return fetchedOrigins[mode], nil
				},
				model: func(context.Context, relay.ClientConfig) (service, error) {
					events = append(events, "model")
					return model, nil
				},
				git: func(_ context.Context, c relay.ClientConfig) (service, error) {
					if c.Token == "" {
						t.Fatal("missing scoped supervisor token")
					}
					events = append(events, "git")
					return git, nil
				},
				start: func(context.Context, []string) (actorRuntime, error) {
					events = append(events, "actor")
					return runtime, nil
				},
				relay: func(ctx context.Context, _ relay.ClientConfig) error { cancel(); <-ctx.Done(); return ctx.Err() },
			}
			t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
			if err := dispatch(ctx, nil, func() []string { return append(validEnv(t), "BROOD_GUEST_STAGE=runtime") }, d); err != nil {
				t.Fatal(err)
			}
			want := []string{"secrets", "origin", "model", "actor"}
			if mode == "gateway" {
				want = []string{"secrets", "origin", "model", "git", "actor"}
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events %v want %v", events, want)
			}
			if mode == "gateway" && !git.closed {
				t.Fatal("git gateway was not closed")
			}
		})
	}
}

func TestOriginOrGitGatewayFailureStopsBeforeActor(t *testing.T) {
	for _, failure := range []string{"origin", "git"} {
		t.Run(failure, func(t *testing.T) {
			network := newFake()
			d := dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) { return network, nil },
				origin: func(context.Context, relay.ClientConfig, []string) (guest.WorkspaceOrigin, error) {
					if failure == "origin" {
						return guest.WorkspaceOrigin{}, errors.New("private-token")
					}
					return fetchedOrigins["gateway"], nil
				},
				git: func(context.Context, relay.ClientConfig) (service, error) {
					if failure == "origin" {
						t.Fatal("Git gateway started without a fetched origin")
					}
					return nil, errors.New("private-token")
				},
				start: func(context.Context, []string) (actorRuntime, error) {
					t.Fatal("actor started without its workspace origin or Git gateway")
					return nil, nil
				},
			}
			t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
			err := dispatch(context.Background(), nil, func() []string { return append(validEnv(t), "BROOD_GUEST_STAGE=runtime") }, d)
			if err == nil || strings.Contains(err.Error(), "private-token") || !network.closed {
				t.Fatalf("unsafe or leaked failure: %v closed=%v", err, network.closed)
			}
		})
	}
}

func TestSecretFailureStopsBeforeOriginFetch(t *testing.T) {
	network := newFake()
	d := dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) { return network, nil },
		secrets: func(context.Context, relay.ClientConfig) error { return errors.New("private-token") },
		origin: func(context.Context, relay.ClientConfig, []string) (guest.WorkspaceOrigin, error) {
			t.Fatal("origin fetched after a failed secrets cold fetch")
			return guest.WorkspaceOrigin{}, nil
		},
	}
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	err := dispatch(context.Background(), nil, func() []string { return append(validEnv(t), "BROOD_GUEST_STAGE=runtime") }, d)
	if err == nil || strings.Contains(err.Error(), "private-token") || !network.closed {
		t.Fatalf("unsafe or leaked failure: %v closed=%v", err, network.closed)
	}
}
