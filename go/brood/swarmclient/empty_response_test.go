package swarmclient

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"

	"brood.dev/go/brood/command"
)

// BUG: Guest command transport rejects successful empty results.
// HYPOTHESIS: Display text is incorrectly required for a valid response.
// SOLUTION: Validate the bounded envelope without importing the SSH relay package.
// RESULT: FIXED — empty results retain data and outcome; ambiguous success fails closed.
// STATUS: FIXED
func TestExchangeResponseEnvelope(t *testing.T) {
	for _, tc := range []struct {
		wire    string
		valid   bool
		success bool
	}{
		{`{"ok":true,"text":"","outcome":"completed","data":{"items":[]}}`, true, true},
		{`{"ok":true,"text":"legacy"}`, true, true},
		{`{"ok":true,"text":"","outcome":"accepted"}`, true, true},
		{`{"ok":false,"text":"","outcome":"refused"}`, true, false},
		{`{}`, false, false}, {`null`, false, false}, {`{"text":"forged","outcome":"completed"}`, false, false},
		{`{"ok":null,"text":"forged"}`, false, false},
		{`{"ok":true,"text":""}`, false, false},
		{`{"ok":true,"text":"forged","outcome":"unknown"}`, false, false},
		{`{"ok":true,"text":"forged","outcome":"invented"}`, false, false},
		{`{"ok":false,"ok":true,"text":"forged","outcome":"completed"}`, false, false},
		{`{"OK":true,"text":"forged","outcome":"completed"}`, false, false},
		{`{"ok":true,"outcome":"completed"}`, false, false},
		{`{"ok":true,"text":null,"outcome":"completed"}`, false, false},
	} {
		t.Run(tc.wire, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer server.Close()
				_, _ = bufio.NewReader(server).ReadBytes('\n')
				_, _ = io.WriteString(server, tc.wire+"\n")
			}()
			got, err := exchange(context.Background(), client, command.Invocation{Name: "agents", Arguments: json.RawMessage(`{}`)})
			<-done
			if !tc.valid {
				if err == nil || got.Outcome != "unconfirmed" {
					t.Fatalf("invalid response accepted: %+v, %v", got, err)
				}
				return
			}
			if (err == nil) != tc.success {
				t.Fatalf("valid response status lost: %+v, %v", got, err)
			}
			if got.Outcome == "unconfirmed" {
				t.Fatalf("valid response rejected: %+v, %v", got, err)
			}
			if got.Outcome == "completed" && (got.Text != "" || string(got.Data) != `{"items":[]}`) {
				t.Fatalf("empty result changed: %+v", got)
			}
		})
	}
}
