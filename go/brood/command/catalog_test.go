package command_test

// FEATURE: Shared command catalog for CLI and MCP adapters.
// SPEC: Exactly 31 tools (25 guest-visible), generated schemas, strict bounded validation, canonical parity.
// IMPLEMENTATION: Per-command declarations share schema generation and argument validation.
// RESULT: IMPLEMENTED — catalog, rejection cases, defaults, and exact numeric parity pass.
// STATUS: DONE

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
)

func TestCanonicalZeroAndInvalidUTF8(t *testing.T) {
	for _, number := range []string{"-0", "-0.0", "0e5"} {
		invocation, err := command.ParseTool("project_limit", json.RawMessage(`{"actors":`+number+`}`))
		if err != nil || string(invocation.Arguments) != `{"actors":0,"name":""}` {
			t.Errorf("zero normalization: %s %v", invocation.Arguments, err)
		}
	}
	bad := string([]byte{0xff})
	if _, err := command.ParseCLI([]string{"add", bad}); err == nil {
		t.Error("CLI accepted invalid UTF-8")
	}
	if _, err := command.ParseTool("add", json.RawMessage("{\"name\":\""+bad+"\"}")); err == nil {
		t.Error("tool accepted invalid UTF-8")
	}
}

func TestExactNumericValidation(t *testing.T) {
	for _, number := range []string{"1.00000000000000001", "0.999999999999999999", "9007199254740991.1", "9007199254740992"} {
		raw := json.RawMessage(`{"actors":` + number + `}`)
		if _, err := command.ParseTool("project_limit", raw); err == nil {
			t.Errorf("accepted noninteger or out-of-range %s", number)
		}
		if _, err := command.ParseCLI([]string{"project", "limit", "--actors", number}); err == nil {
			t.Errorf("CLI accepted %s", number)
		}
	}
	for _, number := range []string{"1e0", "1.00", "0", "9007199254740991"} {
		tool, err := command.ParseTool("project_limit", json.RawMessage(`{"actors":`+number+`}`))
		if err != nil {
			t.Fatal(err)
		}
		cli, err := command.ParseCLI([]string{"project", "limit", "--actors", number})
		if err != nil || !reflect.DeepEqual(tool, cli) {
			t.Fatalf("numeric parity %s: %+v %+v %v", number, tool, cli, err)
		}
	}
}

func TestCoordinationAndTerminalActions(t *testing.T) {
	for _, action := range []string{"show", "take", "give"} {
		if _, err := command.ParseTool("landing", json.RawMessage(`{"action":"`+action+`"}`)); err != nil {
			t.Error(err)
		}
	}
	for _, action := range []string{"open", "read", "input", "interrupt", "resize", "close"} {
		if _, err := command.ParseTool("session_terminal", json.RawMessage(`{"name":"w","action":"`+action+`"}`)); err != nil {
			t.Error(err)
		}
	}
}

func TestCatalog(t *testing.T) {
	names := strings.Fields("agents send dispatch pipeline_visit_complete ask_operator tail slash session_history session_terminal add delete stop start restart update recycle wipe reset project_add project_limit landing node node_drain node_up models profile stat diagnose server_diagnose gitlab_merge_propose gitlab_proposal_get gitlab_mr_create gitlab_operation_get gitlab_issue_get gitlab_ci_get")
	specs := command.Specs()
	if len(specs) != len(names) {
		t.Fatalf("got %d specs", len(specs))
	}
	denied := map[string]bool{"project_add": true, "project_limit": true, "node": true, "node_drain": true, "node_up": true, "server_diagnose": true}
	seen := map[string]bool{}
	for _, name := range names {
		s, ok := command.Lookup(name)
		if !ok || seen[s.ToolName] {
			t.Fatalf("missing or duplicate %s", name)
		}
		seen[s.ToolName] = true
		if strings.Join(s.Path, "_") != name || s.Description == "" {
			t.Fatalf("bad spec: %+v", s)
		}
		if s.Guest == denied[name] {
			t.Errorf("guest %s = %v", name, s.Guest)
		}
		schema := s.Schema()
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Errorf("bad schema %s", name)
		}
		props := schema["properties"].(map[string]any)
		if len(props) != len(s.Args) {
			t.Errorf("schema args %s", name)
		}
		for _, a := range s.Args {
			p := props[a.Name].(map[string]any)
			if p["type"] != a.Type {
				t.Errorf("schema type %s.%s", name, a.Name)
			}
		}
	}
	if _, ok := command.Lookup("nope"); ok {
		t.Fatal("unknown tool accepted")
	}
	// Returned metadata must not let one adapter alter another adapter's validation.
	specs[0].Path[0] = "broken"
	s, _ := command.Lookup("send")
	s.Args[0].Name = "broken"
	s, _ = command.Lookup("send")
	if s.Args[0].Name != "to" {
		t.Fatal("catalog mutated")
	}
	for _, s := range command.Specs() {
		if s.Path[0] == "broken" {
			t.Fatal("path mutated")
		}
	}
}

// FEATURE: Start and reset are catalog lifecycle commands (#149).
// SPEC: start resumes a stopped actor and is not destructive; reset replaces the
// container and workspace, is destructive and carries an explicit confirmation.
// Both are guest-visible like stop/delete; neither is read-only.
func TestStartResetMetadata(t *testing.T) {
	for _, tc := range []struct {
		name        string
		destructive bool
		args        []string
	}{{"start", false, []string{"name"}}, {"reset", true, []string{"name", "confirm"}}} {
		s, ok := command.Lookup(tc.name)
		if !ok {
			t.Fatalf("%s is not declared", tc.name)
		}
		if s.ReadOnly || s.Destructive != tc.destructive || !s.Guest || !reflect.DeepEqual(s.Path, []string{tc.name}) {
			t.Errorf("%s metadata: %+v", tc.name, s)
		}
		names := []string{}
		for _, a := range s.Args {
			names = append(names, a.Name)
		}
		if !reflect.DeepEqual(names, tc.args) {
			t.Errorf("%s args: %v", tc.name, names)
		}
		schema := s.Schema()
		if !reflect.DeepEqual(schema["required"], []string{"name"}) {
			t.Errorf("%s required: %v", tc.name, schema["required"])
		}
		if tc.name == "reset" {
			confirm := schema["properties"].(map[string]any)["confirm"].(map[string]any)
			if confirm["type"] != "boolean" || confirm["default"] != false {
				t.Errorf("reset confirm schema: %v", confirm)
			}
		}
	}
	all, guest := 0, 0
	for _, s := range command.Specs() {
		all++
		if s.Guest {
			guest++
		}
	}
	if all != 35 || guest != 29 {
		t.Fatalf("catalog=%d guest=%d; want 35 and 29", all, guest)
	}
}

func TestParserParity(t *testing.T) {
	cases := []struct {
		name        string
		cli         []string
		input, want string
	}{
		{"agents", []string{"agents"}, `{}`, `{}`},
		{"send", []string{"send", "worker", "hello"}, `{"message":"hello","to":"worker"}`, `{"message":"hello","to":"worker","wait_seconds":0}`},
		{"dispatch", []string{"dispatch", "worker", "hello", "--request-id", "id-1"}, `{"to":"worker","message":"hello","request_id":"id-1"}`, `{"action":"send","message":"hello","request_id":"id-1","to":"worker"}`},
		{"tail", []string{"tail", "worker", "--lines=12", "--grep", "error"}, `{"name":"worker","lines":12,"grep":"error"}`, `{"grep":"error","lines":12,"name":"worker"}`},
		{"session_history", []string{"session", "history", "worker"}, `{"name":"worker"}`, `{"cursor":"","limit":100,"name":"worker"}`},
		{"session_terminal", []string{"session", "terminal", "worker", "read"}, `{"name":"worker","action":"read"}`, `{"action":"read","cols":100,"cursor":"","data":"","mode":"ro","name":"worker","rows":30,"seq":0,"tid":"","wait":0}`},
		{"delete", []string{"delete", "worker", "--confirm"}, `{"name":"worker","confirm":true}`, `{"confirm":true,"name":"worker"}`},
		{"start", []string{"start", "worker"}, `{"name":"worker"}`, `{"name":"worker"}`},
		{"reset", []string{"reset", "worker", "--confirm"}, `{"name":"worker","confirm":true}`, `{"confirm":true,"name":"worker"}`},
		{"reset", []string{"reset", "worker"}, `{"name":"worker"}`, `{"confirm":false,"name":"worker"}`},
		{"project_add", []string{"project", "add", "demo"}, `{"name":"demo"}`, `{"description":"","name":"demo"}`},
		{"project_limit", []string{"project", "limit", "--actors", "0"}, `{"actors":0}`, `{"actors":0,"name":""}`},
		{"project_limit", []string{"project", "limit"}, `{}`, `{"name":""}`},
		{"landing", []string{"landing"}, `{}`, `{"action":"show","name":"","request_id":""}`},
		{"stat", []string{"stat"}, `{}`, `{"days":14,"name":""}`},
		{"slash", []string{"slash", "worker", "--", "--help"}, `{"name":"worker","command":"--help"}`, `{"command":"--help","name":"worker"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.want, func(t *testing.T) {
			tool, err := command.ParseTool(tc.name, json.RawMessage(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			cli, err := command.ParseCLI(tc.cli)
			if err != nil {
				t.Fatal(err)
			}
			if tool.Name != tc.name || string(tool.Arguments) != tc.want || !reflect.DeepEqual(tool, cli) {
				t.Fatalf("tool=%+v cli=%+v want=%s", tool, cli, tc.want)
			}
		})
	}
}

func TestRejectInvalidTool(t *testing.T) {
	cases := []struct{ name, input, reason string }{
		{"missing", `{}`, "unknown"}, {"agents", ``, "object"}, {"agents", `null`, "object"}, {"agents", `[]`, "object"},
		{"agents", `{} {}`, "JSON"}, {"agents", `{"project":"x"}`, "unknown"},
		{"send", `{"to":"w","message":"x","to":"z"}`, "duplicate"},
		{"send", `{"to":"w","message":"x","to":"z"}`, "duplicate"},
		{"send", `{"to":"w"}`, "message"}, {"send", `{"to":"","message":"x"}`, "to"},
		{"send", `{"to":null,"message":"x"}`, "to"}, {"send", `{"to":4,"message":"x"}`, "to"},
		{"send", `{"to":"w","message":"x","wait_seconds":-1}`, "wait_seconds"},
		{"send", `{"to":"w","message":"x","wait_seconds":"1"}`, "wait_seconds"},
		{"dispatch", `{"to":"w","message":"x"}`, "request_id"},
		{"tail", `{"name":"w","lines":1.5}`, "lines"}, {"tail", `{"name":"w","lines":10001}`, "lines"},
		{"tail", `{"name":"w","lines":0}`, "lines"}, {"tail", `{"name":"w","lines":1e1000}`, "lines"},
		{"delete", `{"name":"w","confirm":"true"}`, "confirm"},
		{"start", `{"name":"w","confirm":true}`, "unknown argument"}, {"start", `{}`, "name"},
		{"reset", `{"name":"w","confirm":"true"}`, "confirm"}, {"reset", `{"confirm":true}`, "name"},
		{"project_limit", `{"actors":-1}`, "actors"}, {"project_limit", `{"actors":0.5}`, "actors"},
		{"session_terminal", `{"name":"w","action":"read","mode":"root"}`, "mode"},
		{"agents", `{"x":{"x":{"x":1}}}`, "unknown"},
		{"send", `{"to":"w","message":"` + strings.Repeat("x", 1048576) + `"}`, "large"},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.reason, func(t *testing.T) {
			_, err := command.ParseTool(tc.name, json.RawMessage(tc.input))
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("got %v; want %q", err, tc.reason)
			}
		})
	}
}

func TestRejectInvalidCLI(t *testing.T) {
	for _, argv := range [][]string{
		nil, {"unknown"}, {"session"}, {"agents", "extra"}, {"send", "w"}, {"dispatch", "w", "m"},
		{"tail", "w", "--lines", "-1"}, {"tail", "w", "--lines", "1.5"}, {"tail", "w", "--lines", "2", "--lines", "3"},
		{"delete", "w", "--confirm=yes"}, {"send", "w", "x", "--project", "p"}, {"tail", "w", "--lines"},
		{"send", "w", "x", "--to", "other"}, {"agents", "--unknown"},
	} {
		if _, err := command.ParseCLI(argv); err == nil {
			t.Errorf("accepted %q", argv)
		}
	}
}

func TestEverySpecDefaultsAndSchema(t *testing.T) {
	for _, s := range command.Specs() {
		args := map[string]any{}
		argv := append([]string{}, s.Path...)
		for _, a := range s.Args {
			if a.Required {
				value := "value"
				if len(a.Enum) > 0 {
					value = a.Enum[0]
				}
				args[a.Name] = value
				if a.Type == "integer" {
					value = "1"
					args[a.Name] = 1
				}
				if a.Positional {
					argv = append(argv, value)
				} else {
					argv = append(argv, "--"+strings.ReplaceAll(a.Name, "_", "-"), value)
				}
			}
		}
		raw, _ := json.Marshal(args)
		tool, err := command.ParseTool(s.ToolName, raw)
		if err != nil {
			t.Fatalf("%s: %v", s.ToolName, err)
		}
		cli, err := command.ParseCLI(argv)
		if err != nil {
			t.Fatalf("%s: %v", s.ToolName, err)
		}
		if !reflect.DeepEqual(tool, cli) {
			t.Errorf("%s parity", s.ToolName)
		}
		schema := s.Schema()
		properties := schema["properties"].(map[string]any)
		for _, a := range s.Args {
			if a.Default != nil && properties[a.Name].(map[string]any)["default"] != a.Default {
				t.Errorf("%s default", a.Name)
			}
		}
	}
}

// FEATURE: The dispatch description states the ownership rule (#273).
// SPEC: since #267 a master dispatches only to the workers it added with add;
// the CLI help and the MCP tool description come from this one spec, so the
// model learns the rule before a refusal.
func TestDispatchDescribesOwnership(t *testing.T) {
	s, ok := command.Lookup("dispatch")
	if !ok {
		t.Fatal("dispatch is not declared")
	}
	if !strings.Contains(s.Description, "only to workers it added with add") {
		t.Fatalf("dispatch description lacks the ownership rule: %q", s.Description)
	}
}

// FEATURE: The update description states the controlled restart (#133).
// SPEC: update restarts a running actor in its retained container onto the
// server's current guest release; the profile's template and resources stay.
// It is refused while the actor's swarm runs a pipeline pass, while the actor
// has a dispatch lease or a landing token and when its guest does not report
// its current release, is a no-op on the current release, leaves a stopped
// actor to start, and a master updates only workers it added. The CLI
// help and the MCP tool description come from this one spec, so the model
// learns these rules before a refusal.
func TestUpdateDescribesControlledRestart(t *testing.T) {
	s, ok := command.Lookup("update")
	if !ok {
		t.Fatal("update is not declared")
	}
	for _, rule := range []string{
		"Restart a running actor in its retained container",
		"server's current guest release",
		"template and resources do not change",
		"while the actor's swarm runs a pipeline pass",
		"a dispatch lease or a landing token",
		"when its guest does not report its current release",
		"already runs that release",
		"stopped actor",
		"only workers it added with add",
	} {
		if !strings.Contains(s.Description, rule) {
			t.Errorf("update description lacks %q: %q", rule, s.Description)
		}
	}
	if s.ReadOnly || s.Destructive || !s.Guest {
		t.Fatalf("update annotations: read-only %v, destructive %v, guest %v", s.ReadOnly, s.Destructive, s.Guest)
	}
}
