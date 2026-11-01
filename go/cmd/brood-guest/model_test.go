//go:build linux

package main

import (
	"brood.dev/go/brood/proxmox/relay"
	"context"
	"testing"
)

func TestModelGatewayStartsBeforeActorAndClosesOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, runtime, model := newFake(), newFake(), newFake()
	started := false
	d := dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) { return network, nil },
		model: func(_ context.Context, c relay.ClientConfig) (service, error) {
			if c.Token == "" {
				t.Fatal("missing scoped supervisor token")
			}
			started = true
			return model, nil
		},
		start: func(context.Context, []string) (actorRuntime, error) {
			if !started {
				t.Fatal("actor started before model gateway")
			}
			return runtime, nil
		},
		relay: func(ctx context.Context, _ relay.ClientConfig) error { cancel(); <-ctx.Done(); return ctx.Err() },
	}
	if err := dispatch(ctx, nil, func() []string { return validEnv(t) }, d); err != nil {
		t.Fatal(err)
	}
	if !model.closed || !runtime.closed || !network.closed {
		t.Fatal("guest services were not closed")
	}
}
