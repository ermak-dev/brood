//go:build linux

package guest

import (
	"slices"
	"testing"
)

// FEATURE: The browser attaches to the owned raw PTY, not a tmux viewport (#162).
// SPEC: ttyd keeps its fixed loopback/origin boundary and executes only the
// trusted guest attach mode. Neither browser attach nor disconnect starts Claude.
func TestBrowserTerminalUsesDirectPTYAttach(t *testing.T) {
	args := actorArgs("/opt/brood/bin/brood-guest", true)
	for _, forbidden := range []string{"tmux", "new-session", "--brood-guest-app"} {
		if slices.Contains(args, forbidden) {
			t.Fatalf("browser still launches terminal virtualization or the app: %v", args)
		}
	}
	if !slices.Contains(args, "--brood-guest-attach") {
		t.Fatalf("fixed PTY attach target missing: %v", args)
	}
	for _, required := range []string{"--interface", "127.0.0.1", "--port", "8080", "--writable", "--check-origin"} {
		if !slices.Contains(args, required) {
			t.Fatalf("terminal boundary lost %s", required)
		}
	}
}
