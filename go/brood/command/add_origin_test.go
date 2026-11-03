package command_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
)

// FEATURE: add accepts an optional Git origin (#146).
// SPEC: origin is an optional string flag bounded to 1024 characters; an absent
// origin stays absent so the server applies the master or project default.
func TestAddOriginArgument(t *testing.T) {
	spec, ok := command.Lookup("add")
	if !ok || !spec.Guest || spec.ReadOnly || spec.Destructive {
		t.Fatalf("add spec: %+v", spec)
	}
	properties := spec.Schema()["properties"].(map[string]any)
	origin, ok := properties["origin"].(map[string]any)
	if !ok || origin["type"] != "string" || origin["maxLength"] != 1024 || origin["default"] != nil {
		t.Fatalf("origin schema: %v", properties)
	}
	if required := spec.Schema()["required"].([]string); !reflect.DeepEqual(required, []string{"name"}) {
		t.Fatalf("required: %v", required)
	}
	for _, tc := range []struct {
		cli         []string
		input, want string
	}{
		{[]string{"add", "worker"}, `{"name":"worker"}`, `{"name":"worker"}`},
		{[]string{"add", "worker", "--origin", "https://github.com/owner/repo"}, `{"name":"worker","origin":"https://github.com/owner/repo"}`, `{"name":"worker","origin":"https://github.com/owner/repo"}`},
		{[]string{"add", "--origin=https://github.com/owner/repo", "worker"}, `{"origin":"https://github.com/owner/repo","name":"worker"}`, `{"name":"worker","origin":"https://github.com/owner/repo"}`},
	} {
		tool, err := command.ParseTool("add", json.RawMessage(tc.input))
		if err != nil {
			t.Fatal(err)
		}
		cli, err := command.ParseCLI(tc.cli)
		if err != nil {
			t.Fatal(err)
		}
		if string(tool.Arguments) != tc.want || !reflect.DeepEqual(tool, cli) {
			t.Fatalf("tool=%s cli=%s want=%s", tool.Arguments, cli.Arguments, tc.want)
		}
	}
	for _, input := range []string{
		`{"name":"worker","origin":null}`,
		`{"name":"worker","origin":1}`,
		`{"name":"worker","origin":"` + strings.Repeat("a", 1025) + `"}`,
		`{"name":"worker","Origin":"https://github.com/owner/repo"}`,
	} {
		if _, err := command.ParseTool("add", json.RawMessage(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
