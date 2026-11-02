//go:build linux

package guest

// FEATURE: Fixed session helper UID boundary.
// SPEC: No actor-provided executable, path or environment; helper request bounded
// and strict; root launches the same immutable executable with UID/GID 1000.
// IMPLEMENTATION: Fixed helper spec and bounded JSON across the UID boundary.
// RESULT: IMPLEMENTED — helper input and spec checked without a live guest.
// STATUS: DONE

import (
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func TestSessionRestartRetainsFixedGateway(t *testing.T) {
	env := strings.Join(sessionHelperSpec().Env, "\n")
	for _, required := range []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:8317", "ANTHROPIC_AUTH_TOKEN=brood-local", "BROOD_EXECUTION_MODE=guest"} {
		if !strings.Contains(env, required) {
			t.Fatalf("restart helper lost fixed gateway setting %s", required)
		}
	}
}

func TestSessionHelperSpecAndInput(t *testing.T) {
	spec := sessionHelperSpec()
	if spec.Executable != "/proc/self/exe" {
		t.Fatalf("%#v", spec)
	}
	if len(spec.Args) != 1 || spec.Args[0] != "--brood-guest-session" {
		t.Fatal(spec.Args)
	}
	req, err := ReadSessionRequest(strings.NewReader(`{"operation":"tail","arguments":{"name":"routing"},"caller":"user:42"}`))
	if err != nil || req.Operation != "tail" {
		t.Fatalf("%#v %v", req, err)
	}
	for _, bad := range []string{`{"operation":"tail","arguments":{},"caller":"a","executable":"/tmp/evil"}`, `{} {}`, strings.Repeat("a", relay.MaxSessionBytes+1)} {
		if _, err := ReadSessionRequest(strings.NewReader(bad)); err == nil {
			t.Fatal("unsafe input accepted")
		}
	}
}
