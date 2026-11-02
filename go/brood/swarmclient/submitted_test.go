package swarmclient

import "testing"

// BUG: The fixed broker client must preserve accepted input that is not confirmed complete.
// SPEC: A submitted outcome remains submitted, not a transport failure or completed command.
func TestClientPreservesSubmittedOutcome(t *testing.T) {
	response, ok := decodeResponse([]byte(`{"ok":true,"text":"input submitted; command completion is not confirmed","outcome":"submitted"}`))
	if !ok || !response.OK || response.Outcome != "submitted" {
		t.Fatalf("submitted result lost: %+v valid=%v", response, ok)
	}
}
