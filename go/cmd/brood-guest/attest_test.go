//go:build linux

package main

// FEATURE: Workspace attestation wiring (#183).
// SPEC: The relay session handler signs with the bootstrap identity and stops
// the actor through the running runtime, never through a separate path.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest"
	"brood.dev/go/brood/proxmox/relay"
)

type quiesceRuntime struct {
	*fakeService
	calls int
}

func (q *quiesceRuntime) Quiesce(context.Context, func(context.Context) error) error {
	q.calls++
	return errors.New("quiesce refused in test")
}

func TestWorkspaceAttestationUsesRuntimeQuiesce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, runtime := newFake(), &quiesceRuntime{fakeService: newFake()}
	origin := "https://git.example.com/group/project"
	args, _ := json.Marshal(map[string]string{"swarm": "01JSWARM0000000000000000AA", "step": "request-1", "origin": origin, "commit": strings.Repeat("ab", 20), "challenge": base64.RawURLEncoding.EncodeToString(make([]byte, 32))})
	var got relay.SessionResponse
	d := dependencies{euid: func() int { return 0 }, network: func(context.Context) (service, error) { return network, nil },
		origin: func(context.Context, relay.ClientConfig, []string) (guest.WorkspaceOrigin, error) {
			return guest.WorkspaceOrigin{Origin: origin, Mode: "direct"}, nil
		},
		start: func(context.Context, []string) (actorRuntime, error) { return runtime, nil },
		relay: func(ctx context.Context, c relay.ClientConfig) error {
			got = c.SessionFactory(ctx)(ctx, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: args, Caller: "server"})
			cancel()
			return ctx.Err()
		}}
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	if err := dispatch(ctx, nil, func() []string { return append(validEnv(t), "BROOD_GUEST_STAGE=runtime") }, d); err != nil {
		t.Fatal(err)
	}
	if runtime.calls != 1 || got.OK || !strings.Contains(got.Text, "quiesce refused in test") {
		t.Fatalf("attestation not bound to the runtime: calls %d, %+v", runtime.calls, got)
	}
}
