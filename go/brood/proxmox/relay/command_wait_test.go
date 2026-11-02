package relay

import (
	"encoding/json"
	"testing"
)

// FEATURE: Canonical arguments preserve caller deadlines across broker and relay.
// SPEC: Receipt and terminal waits are extracted from validated command arguments.
func TestSwarmRequestWait(t *testing.T) {
	for _, tc := range []struct {
		req  SwarmRequest
		want int
	}{
		{SwarmRequest{Tool: "send", WaitSeconds: 300}, 300},
		{SwarmRequest{Tool: "send", Arguments: json.RawMessage(`{"wait_seconds":300}`)}, 300},
		{SwarmRequest{Tool: "session_terminal", Arguments: json.RawMessage(`{"wait":15}`)}, 15},
		{SwarmRequest{Tool: "profile", Arguments: json.RawMessage(`{}`)}, 0},
	} {
		if got := SwarmWaitSeconds(tc.req); got != tc.want {
			t.Errorf("wait=%d want=%d", got, tc.want)
		}
	}
}
