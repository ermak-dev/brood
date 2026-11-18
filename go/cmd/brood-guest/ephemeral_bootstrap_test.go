//go:build linux

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: Supervisor не запускает свободную модель эфемерного worker (#292).
// SPEC: Relay и сервисы доступны, bootstrap pin передан root attestor, но первый
// model session возможен только после подготовки/checkout; persistent не меняется.
// IMPLEMENTATION: Ephemeral исключён из autorun; root attestor получает bootstrap pin.
// RESULT: IMPLEMENTED
// STATUS: DONE
type ephemeralBootRuntime struct {
	*fakeService
	quiesces atomic.Int32
}

func (r *ephemeralBootRuntime) Quiesce(context.Context, func(context.Context) error) error {
	r.quiesces.Add(1)
	return errors.New("unexpected quiesce")
}

func TestSupervisorPausesEphemeralFreeAutostartUntilRootAttestation(t *testing.T) {
	for _, ephemeral := range []bool{false, true} {
		name := "persistent"
		if ephemeral {
			name = "ephemeral"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			network, runtime := newFake(), &ephemeralBootRuntime{fakeService: newFake()}
			var autostarts atomic.Int32
			var response relay.SessionResponse
			origin := guest.WorkspaceOrigin{Origin: "https://git.example.org/group/repo.git", Mode: "gateway"}
			if ephemeral {
				origin.Ephemeral = &guest.EphemeralWorkspace{Commit: strings.Repeat("a", 40)}
			}
			d := dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) { return network, nil },
				origin:  func(context.Context, relay.ClientConfig, []string) (guest.WorkspaceOrigin, error) { return origin, nil },
				start:   func(context.Context, []string) (actorRuntime, error) { return runtime, nil },
				autorun: func(context.Context) { autostarts.Add(1) },
				relay: func(ctx context.Context, config relay.ClientConfig) error {
					if config.SessionFactory == nil {
						t.Error("root session factory unavailable")
					} else if ephemeral {
						args, _ := json.Marshal(map[string]string{"swarm": "swarm", "step": "child", "origin": origin.Origin, "commit": strings.Repeat("b", 40), "challenge": base64.RawURLEncoding.EncodeToString(make([]byte, 32))})
						response = config.SessionFactory(ctx)(ctx, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: args, Caller: "server"})
					}
					cancel()
					return ctx.Err()
				}}
			t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
			if err := dispatch(ctx, nil, func() []string { return append(validEnv(t), "BROOD_GUEST_STAGE=runtime") }, d); err != nil {
				t.Fatal(err)
			}
			want := int32(1)
			if ephemeral {
				want = 0
			}
			if autostarts.Load() != want {
				t.Fatalf("free model autostarts=%d, want=%d", autostarts.Load(), want)
			}
			if ephemeral && (response.OK || !strings.Contains(response.Text, "bootstrap commit") || runtime.quiesces.Load() != 0) {
				t.Fatalf("supervisor lost negotiated pin: response=%+v effects=%d", response, runtime.quiesces.Load())
			}
			if !network.closed || !runtime.closed {
				t.Fatal("paused supervisor leaked services")
			}
		})
	}
}
