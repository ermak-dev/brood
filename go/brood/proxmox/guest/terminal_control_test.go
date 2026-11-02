//go:build linux

package guest

import (
	"context"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: Slash and workspace mutations share the private terminal writer gate.
// SPEC: Non-sequenced control cannot bypass an existing writer attachment.
func TestSessionControlRespectsTerminalWriter(t *testing.T) {
	f := terminalFixtureNew(t)
	terminalPage(t, terminalCall(f.c, "owner", `{"action":"open","mode":"rw"}`))
	called := false
	got := f.c.Control(context.Background(), func() relay.SessionResponse {
		called = true
		return relay.SessionResponse{OK: true, Text: "changed", Outcome: "complete"}
	})
	if got.OK || called {
		t.Fatalf("writer bypassed: %+v called=%v", got, called)
	}
}
