package server

import (
	"brood.dev/go/brood/command"
	"testing"
)

// FEATURE: Structured command outcomes survive legacy broker response checks.
// SPEC: Empty successful reads are not transport failures; uncertain or failed outcomes never imply success.
func TestCommandSwarmOutcome(t *testing.T) {
	empty := commandSwarmResponse(command.Result{Outcome: "completed"})
	if !empty.OK || empty.Text == "" {
		t.Fatalf("empty read lost: %+v", empty)
	}
	for _, outcome := range []string{"unknown", "unconfirmed", "failed", "refused", "unsupported"} {
		if response := commandSwarmResponse(command.Result{Text: "result", Outcome: outcome}); response.OK {
			t.Errorf("outcome %s reported success", outcome)
		}
	}
}
