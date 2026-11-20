//go:build linux

package guest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

// BUG: The legacy session replacement helper would stop the app and then
// try to ensure it on the same already consumed one-shot holder.
func TestInboxSessionReplacementRefusesBeforeTouchingTheHeldApp(t *testing.T) {
	for _, op := range []string{"restart", "recycle"} {
		t.Run(op, func(t *testing.T) {
			var events []string
			cfg := testConfig(&events)
			withHeldPTYTestLauncher(cfg, func(*heldPTYTestProcess) {})
			r, err := Start(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			connection, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := inboxPTYTestAttestor(r, &attestRecorder{})
			called := 0
			handler := newSessionHandler(connection, nil, a, func(context.Context, relay.SessionRequest) relay.SessionResponse {
				called++
				return relay.SessionResponse{OK: true, Outcome: "complete"}
			})
			attested := handler(connection, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil)})
			launch := boundActorLaunch(r)
			if !attested.OK || attested.InboxLaunch == nil || launch == nil {
				t.Fatalf("fixture did not start a held inbox app: %+v", attested)
			}
			freed := 0
			a.Free = func() error { freed++; return nil }
			answer := handler(connection, relay.SessionRequest{Operation: op, Arguments: json.RawMessage(`{"confirm":true}`)})
			if answer.OK || !strings.Contains(answer.Text, "held inbox") || called != 0 || freed != 0 || boundActorLaunch(r) != launch {
				t.Fatalf("legacy replacement touched the held inbox app: %+v (helper=%d free=%d)", answer, called, freed)
			}
			if err := r.Quiesce(connection, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			answer = handler(connection, relay.SessionRequest{Operation: op, Arguments: json.RawMessage(`{"confirm":true}`)})
			if !answer.OK || called != 1 || freed != 1 {
				t.Fatalf("ordinary replacement was blocked after retirement: %+v", answer)
			}
		})
	}
}
