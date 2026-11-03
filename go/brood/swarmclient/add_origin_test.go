package swarmclient

import (
	"encoding/json"
	"reflect"
	"testing"

	"brood.dev/go/brood/command"
)

// FEATURE: Guest add keeps an explicit Git origin (#146).
// SPEC: Name-only add keeps the legacy {"tool","to"} wire; an add with origin
// uses canonical arguments so the origin is never silently dropped.
func TestAddRequestKeepsOrigin(t *testing.T) {
	legacy, err := NewRequest(command.Invocation{Name: "add", Arguments: json.RawMessage(`{"name":"peer"}`)})
	if err != nil || !reflect.DeepEqual(legacy, Request{Tool: "add", To: "peer"}) {
		t.Fatalf("legacy add: %+v %v", legacy, err)
	}
	withOrigin, err := NewRequest(command.Invocation{Name: "add", Arguments: json.RawMessage(`{"origin":"https://github.com/owner/repo","name":"peer"}`)})
	if err != nil || withOrigin.Tool != "add" || withOrigin.To != "" || string(withOrigin.Arguments) != `{"name":"peer","origin":"https://github.com/owner/repo"}` {
		t.Fatalf("origin add: %+v %v", withOrigin, err)
	}
}
