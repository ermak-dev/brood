package relay

import "testing"

// BUG: A successful slash submission is not command completion, but is still a valid transport result.
// SPEC: Preserve the submitted outcome without upgrading it to completed.
func TestSwarmPreservesSubmittedOutcome(t *testing.T) {
	response, ok := DecodeSwarmResponse([]byte(`{"ok":true,"text":"input submitted; command completion is not confirmed","outcome":"submitted"}`))
	if !ok || !response.OK || response.Outcome != "submitted" {
		t.Fatalf("submitted result lost: %+v valid=%v", response, ok)
	}
}
