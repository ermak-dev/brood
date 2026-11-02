package command

import (
	"encoding/json"
	"strings"
	"testing"
)

// FEATURE: Preserve delivery waiting semantics during the CLI migration.
// SPEC: Send waits up to 300 seconds for receipts/idle, never for an assistant reply.
func TestCanonicalArgumentsStayWithinInputLimit(t *testing.T) {
	input := json.RawMessage(`{"to":"worker","message":"` + strings.Repeat("<", 200000) + `"}`)
	inv, err := ParseTool("send", input)
	if err != nil {
		return
	}
	if len(inv.Arguments) > MaxArgumentsBytes {
		t.Fatalf("canonical arguments exceed limit: %d", len(inv.Arguments))
	}
	if _, err := ParseTool(inv.Name, inv.Arguments); err != nil {
		t.Fatalf("valid invocation cannot be revalidated: %v", err)
	}
}

func TestDispatchCanReleaseItsAllocation(t *testing.T) {
	if _, err := ParseCLI([]string{"dispatch", "worker", "--request-id", "task-1", "--action", "release"}); err != nil {
		t.Fatal(err)
	}
}

func TestSendPreservesDeliveryWaitContract(t *testing.T) {
	if _, err := ParseTool("send", json.RawMessage(`{"to":"worker","message":"task","wait_seconds":300}`)); err != nil {
		t.Fatal(err)
	}
	spec, _ := Lookup("send")
	if strings.Contains(spec.Description, "reply") {
		t.Fatal("send promises a reply rather than delivery status")
	}
}
