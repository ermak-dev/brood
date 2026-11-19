//go:build linux

package main

// FEATURE: A root supervisor explicitly opts a guest into durable inbox custody.
// SPEC: Parsing is inert, and missing durable state fails before actor or relay startup.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

const testInboxOptIn = "BROOD_GUEST_SUPERVISOR_INBOX"

func TestSupervisorInboxOptInIsInertUntilRuntimePreparation(t *testing.T) {
	env := validEnv(t)
	for _, bad := range []string{"", "0", "true", "1 "} {
		if _, err := parseConfig(replace(env, testInboxOptIn, bad)); err == nil {
			t.Fatalf("accepted non-canonical inbox opt-in %q", bad)
		}
	}
	c, err := parseConfig(replace(env, testInboxOptIn, "1"))
	if err != nil {
		t.Fatalf("explicit inbox opt-in rejected: %v", err)
	}
	runtime, err := parseConfig(runtimeEnv(c))
	if err != nil || runtime.stage != "runtime" || !runtime.stepLaunchBinding || runtime.relay.InboxReceiverID != "" || runtime.relay.HandleInbox != nil {
		t.Fatalf("opt-in was lost across bootstrap or advertised before startup: %v", err)
	}
}

func TestSupervisorInboxOptInWithoutDurableStateNeverStartsAnActor(t *testing.T) {
	network := newFake()
	deps := dependencies{
		euid:              func() int { return 0 },
		network:           func(context.Context) (service, error) { return network, nil },
		deliveryDirectory: func(context.Context) (string, error) { return "", nil },
		start: func(context.Context, []string) (actorRuntime, error) {
			t.Error("actor started without inbox storage")
			return nil, errors.New("unexpected actor startup")
		},
		relay: func(context.Context, relay.ClientConfig) error {
			t.Error("relay started without inbox storage")
			return errors.New("unexpected relay startup")
		},
	}
	env := append(validEnv(t), "BROOD_GUEST_STAGE=runtime", testInboxOptIn+"=1")
	t.Setenv("PATH", "/usr/bin:/bin")
	err := dispatch(context.Background(), nil, func() []string { return env }, deps)
	if err == nil || !strings.Contains(err.Error(), "inbox requires durable delivery state") || !network.closed {
		t.Fatalf("missing inbox storage did not refuse startup with a reason: %v", err)
	}
}
