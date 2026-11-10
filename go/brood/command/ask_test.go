package command_test

import (
	"encoding/json"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
)

// FEATURE: Actors ask the swarm operator through the guest catalog (#210).
// SPEC: ask_operator is a guest command with only question, kind, context and
// options; identity fields and answer fields are unknown arguments, so the
// caller cannot speak for another actor or answer through this command.
// No guest command carries an answer response.
func TestAskOperatorSpec(t *testing.T) {
	s, ok := command.Lookup("ask_operator")
	if !ok || !s.Guest || s.ReadOnly || s.Destructive {
		t.Fatalf("ask_operator metadata: %+v %v", s, ok)
	}
	inv, err := command.ParseTool("ask_operator", json.RawMessage(`{"question":"Which database?"}`))
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if err := json.Unmarshal(inv.Arguments, &args); err != nil || args["kind"] != "text" || args["question"] != "Which database?" {
		t.Fatalf("defaults: %s %v", inv.Arguments, err)
	}
	if _, err := command.ParseTool("ask_operator", json.RawMessage(`{"question":"q","kind":"poll"}`)); err == nil {
		t.Fatal("unknown kind accepted")
	}
	for _, forged := range []string{"actor_uid", "incarnation", "swarm_id", "response", "outcome", "request_id", "state"} {
		raw := json.RawMessage(`{"question":"q","` + forged + `":"x"}`)
		if _, err := command.ParseTool("ask_operator", raw); err == nil || !strings.Contains(err.Error(), forged) {
			t.Errorf("%s accepted: %v", forged, err)
		}
	}
	cli, err := command.ParseCLI([]string{"ask", "operator", "Ship it?", "--kind", "choice", "--options", "Ship\nWait"})
	if err != nil || cli.Name != "ask_operator" {
		t.Fatalf("cli: %+v %v", cli, err)
	}
	for _, spec := range command.Specs() {
		for _, a := range spec.Args {
			if spec.Guest && a.Name == "response" {
				t.Errorf("guest command %s carries an answer response", spec.ToolName)
			}
		}
	}
}
