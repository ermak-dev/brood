package relay

import (
	"encoding/json"
	"testing"
	"time"
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

// TestSwarmCallTimeout — предел запроса роя на звеньях broker гостя и relay
// сервера (#260): ожидание запроса и запас 15 секунд, а без ожидания —
// квитанция доставки адресата (command.ReceiptWait) и тот же запас.
func TestSwarmCallTimeout(t *testing.T) {
	for _, tc := range []struct {
		req  SwarmRequest
		want time.Duration
	}{
		{SwarmRequest{Tool: "send", To: "peer", Message: "hello"}, 30 * time.Second},
		{SwarmRequest{Tool: "send", WaitSeconds: 300}, 315 * time.Second},
		{SwarmRequest{Tool: "session_terminal", Arguments: json.RawMessage(`{"wait":15}`)}, 30 * time.Second},
		{SwarmRequest{Tool: "profile", Arguments: json.RawMessage(`{}`)}, 30 * time.Second},
	} {
		if got := SwarmCallTimeout(tc.req); got != tc.want {
			t.Errorf("%s: timeout %v, want %v", tc.req.Tool, got, tc.want)
		}
	}
}
