//go:build linux

package main

// FEATURE: Boot-time Claude session (#148).
// SPEC: The fixed autostart helper dispatches before bootstrap env; the root
// supervisor starts the session after secrets, origin and actor services, and
// an autostart that gives up never stops the supervisor.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

func TestAutostartHelperDispatch(t *testing.T) {
	called := false
	d := dependencies{boot: func(context.Context) error { called = true; return nil }}
	err := dispatch(context.Background(), []string{"--brood-guest-autostart"}, func() []string { t.Fatal("autostart helper read bootstrap env"); return nil }, d)
	if err != nil || !called {
		t.Fatal("missing autostart dispatch", err)
	}
}

func TestSupervisorAutostartsAfterActorAndSurvivesFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := newFake()
	events := make(chan string, 8)
	d := dependencies{euid: func() int { return 0 }, secrets: func(context.Context, relay.ClientConfig) error { events <- "secrets"; return nil },
		workspace: func(context.Context, relay.ClientConfig) error {
			events <- "origin"
			return nil
		},
		start: func(context.Context) (actorRuntime, error) { events <- "actor"; return runtime, nil },
		relay: func(ctx context.Context, _ relay.ClientConfig) error {
			// Автостарт уже вернулся неудачей; supervisor обязан жить дальше.
			select {
			case <-time.After(100 * time.Millisecond):
				cancel()
			case <-ctx.Done():
			}
			<-ctx.Done()
			return ctx.Err()
		},
		autorun: func(context.Context) { events <- "autostart" },
	}
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	if err := dispatch(ctx, nil, func() []string { return append(validEnv(t), "BROOD_GUEST_STAGE=runtime") }, d); err != nil {
		t.Fatal(err)
	}
	close(events)
	var got []string
	for event := range events {
		got = append(got, event)
	}
	if !reflect.DeepEqual(got, []string{"secrets", "origin", "actor", "autostart"}) {
		t.Fatalf("events %v", got)
	}
	if !runtime.closed {
		t.Fatal("services leaked")
	}
}
