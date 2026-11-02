//go:build linux

package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: The public terminal utility and helper use the same canonical arguments.
// SPEC: Defaults and opaque snapshot cursors survive both CLI/MCP and controller validation.
func TestTerminalReadWaitsForChangedSnapshot(t *testing.T) {
	f := terminalFixtureNew(t)
	original := f.c.step
	reads := 0
	f.c.step = func(ctx context.Context, step sessionctl.TerminalStep) relay.SessionResponse {
		if step.Action == "read" {
			reads++
			if reads >= 3 {
				f.output = "updated"
			}
		}
		return original(ctx, step)
	}
	opened := terminalPage(t, terminalCall(f.c, "operator", `{"action":"open"}`))
	first := terminalPage(t, terminalCall(f.c, "operator", fmt.Sprintf(`{"action":"read","tid":%q,"seq":1}`, opened.TID)))
	second := terminalPage(t, terminalCall(f.c, "operator", fmt.Sprintf(`{"action":"read","tid":%q,"seq":2,"cursor":%q,"wait":1}`, opened.TID, first.Cursor)))
	if second.Output != "updated" || !second.Reset {
		t.Fatalf("read wait did not observe change: %+v", second)
	}
}

func TestTerminalCatalogArgumentsRoundTrip(t *testing.T) {
	f := terminalFixtureNew(t)
	inv, err := command.ParseTool("session_terminal", json.RawMessage(`{"name":"worker","action":"open"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := f.c.Handle(context.Background(), relay.SessionRequest{Operation: inv.Name, Arguments: inv.Arguments, Caller: "operator"})
	page := terminalPage(t, got)
	args := fmt.Sprintf(`{"name":"worker","action":"read","tid":%q,"seq":1,"cursor":%q}`, page.TID, page.Cursor)
	inv, err = command.ParseTool("session_terminal", json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	terminalPage(t, f.c.Handle(context.Background(), relay.SessionRequest{Operation: inv.Name, Arguments: inv.Arguments, Caller: "operator"}))
}
