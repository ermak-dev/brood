//go:build linux

package guest

import (
	"bufio"
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

// BUG: Broker client rejects legitimate empty completed display text.
// HYPOTHESIS: Nonempty text is used instead of validating the response status.
// SOLUTION: Use the bounded relay response decoder instead of requiring text.
// RESULT: FIXED — Unix socket replies preserve empty results and reject invalid status.
// STATUS: FIXED
func TestBrokerResponseEnvelope(t *testing.T) {
	for _, tc := range []struct {
		wire  string
		valid bool
	}{
		{`{"ok":true,"text":"","outcome":"completed","data":{"items":[]}}`, true},
		{`{"ok":true,"text":"legacy"}`, true},
		{`{"ok":false,"text":"","outcome":"refused"}`, true},
		{`{}`, false}, {`null`, false}, {`{"text":"forged","outcome":"completed"}`, false},
		{`{"ok":null,"text":"forged"}`, false},
		{`{"ok":true,"text":""}`, false},
		{`{"ok":true,"text":"forged","outcome":"unknown"}`, false},
		{`{"ok":true,"text":"forged","outcome":"invented"}`, false},
		{`{"ok":false,"ok":true,"text":"forged","outcome":"completed"}`, false},
		{`{"OK":true,"text":"forged","outcome":"completed"}`, false},
		{`{"ok":true,"outcome":"completed"}`, false},
		{`{"ok":true,"text":null,"outcome":"completed"}`, false},
	} {
		t.Run(tc.wire, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "broker.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				c, err := listener.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				_, _ = bufio.NewReader(c).ReadBytes('\n')
				_, _ = io.WriteString(c, tc.wire+"\n")
			}()
			got := callSwarmBroker(context.Background(), path, relay.SwarmRequest{Tool: "agents"})
			<-done
			if tc.valid {
				if got.Text == "swarm broker unavailable" {
					t.Fatalf("valid response rejected: %+v", got)
				}
				if got.Outcome == "completed" && (got.Text != "" || string(got.Data) != `{"items":[]}`) {
					t.Fatalf("empty result changed: %+v", got)
				}
			} else if got.OK || got.Text != "swarm broker unavailable" {
				t.Fatalf("invalid response accepted: %+v", got)
			}
		})
	}
}
