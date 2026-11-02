package relay

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// BUG: Transport discards completed responses whose display text is empty.
// HYPOTHESIS: Receivers confuse display text with envelope validity.
// SOLUTION: Validate explicit status, unique envelope keys, and known success outcomes.
// RESULT: FIXED — real SSH preserves empty completed text and structured data.
// STATUS: FIXED
func TestSwarmEmptyCompletedSSHRoundTrip(t *testing.T) {
	f := setup(t)
	calls := make(chan SwarmCaller, 1)
	f.cfg.OnSwarmSession = func(ctx context.Context, call SwarmCaller) { calls <- call; <-ctx.Done() }
	data := json.RawMessage(`{"items":[]}`)
	f.h.SetSwarmHandler(func(context.Context, string, int64, SwarmRequest) SwarmResponse {
		return SwarmResponse{OK: true, Text: "", Outcome: "completed", Data: data}
	})
	start(t, f, f.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	select {
	case call := <-calls:
		got := call(ctx, SwarmRequest{Tool: "agents"})
		if !got.OK || got.Text != "" || got.Outcome != "completed" || string(got.Data) != string(data) {
			t.Fatalf("empty completed response lost: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("no SSH session caller")
	}
}

type responseSSHConn struct {
	ssh.Conn
	wire string
}

func (c responseSSHConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	return true, []byte(c.wire), nil
}

func TestSwarmResponseEnvelope(t *testing.T) {
	for _, tc := range []struct {
		wire  string
		valid bool
	}{
		{`{"ok":true,"text":"","outcome":"completed","data":{"items":[]}}`, true},
		{`{"ok":true,"text":"legacy"}`, true},
		{`{"ok":true,"text":"","outcome":"accepted"}`, true},
		{`{"ok":false,"text":"","outcome":"refused"}`, true},
		{`{}`, false}, {`null`, false}, {`{"text":"forged","outcome":"completed"}`, false},
		{`{"ok":null,"text":"forged","outcome":"completed"}`, false},
		{`{"ok":true,"text":""}`, false},
		{`{"ok":true,"text":"forged","outcome":"unknown"}`, false},
		{`{"ok":true,"text":"forged","outcome":"invented"}`, false},
		{`{"ok":false,"ok":true,"text":"forged","outcome":"completed"}`, false},
		{`{"OK":true,"text":"forged","outcome":"completed"}`, false},
		{`{"ok":true,"outcome":"completed"}`, false},
		{`{"ok":true,"text":null,"outcome":"completed"}`, false},
		{`{"ok":true,"text":"x"} {}`, false},
	} {
		t.Run(tc.wire, func(t *testing.T) {
			got := swarmCaller(responseSSHConn{wire: tc.wire}, context.Background())(context.Background(), SwarmRequest{Tool: "agents"})
			if tc.valid {
				if got.Text == "swarm service unavailable or request refused" {
					t.Fatalf("valid response rejected: %+v", got)
				}
			} else if got.OK || got.Text != "swarm service unavailable or request refused" {
				t.Fatalf("invalid response accepted: %+v", got)
			}
		})
	}
}
