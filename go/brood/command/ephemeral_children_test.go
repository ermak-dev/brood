package command_test

// FEATURE: Явные гостевые команды очереди ephemeral jobs (#292).
// SPEC: CLI и tools имеют одну закрытую схему без выбора личности или policy;
// persistent child_create сохраняет прежнюю грамматику.
// IMPLEMENTATION: Две декларации каталога используют общий parser CLI/tools.
// RESULT: IMPLEMENTED — registry/CLI и focused race проходят.
// STATUS: DONE

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
)

func TestEphemeralChildCommandSchemaAndCLI(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		cli       []string
		read      bool
	}{
		{"child_spawn", `{"parent_sequence":1,"parent_request_id":"parent","request_id":"one","task":"Run tests","total":2,"pool_uid":"pool"}`, []string{"child", "spawn", "--parent-sequence", "1", "--parent-request-id", "parent", "--request-id", "one", "--task", "Run tests", "--total", "2", "--pool-uid", "pool"}, false},
		{"child_job", `{"job_id":"job"}`, []string{"child", "job", "--job-id", "job"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ok := command.Lookup(tc.name)
			if !ok || !s.Guest || s.ReadOnly != tc.read || s.Destructive {
				t.Fatalf("missing guest metadata: %+v", s)
			}
			tool, err := command.ParseTool(tc.name, json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			cli, err := command.ParseCLI(tc.cli)
			if err != nil || !reflect.DeepEqual(cli, tool) {
				t.Fatalf("CLI/tool mismatch: %+v %+v %v", cli, tool, err)
			}
			var args map[string]json.RawMessage
			_ = json.Unmarshal(tool.Arguments, &args)
			for key := range args {
				copy := map[string]json.RawMessage{}
				for k, v := range args {
					if k != key {
						copy[k] = v
					}
				}
				raw, _ := json.Marshal(copy)
				if _, err := command.ParseTool(tc.name, raw); err == nil {
					t.Errorf("missing %s accepted", key)
				}
			}
			for _, key := range strings.Fields("role executor_uid actor_uid nonce ctid mode quorum child_id") {
				raw := strings.TrimSuffix(tc.raw, "}") + `,"` + key + `":"forged"}`
				if _, err := command.ParseTool(tc.name, json.RawMessage(raw)); err == nil {
					t.Errorf("forged %s accepted", key)
				}
			}
		})
	}
	for _, total := range []string{"0", "65", "1.5"} {
		raw := `{"parent_sequence":1,"parent_request_id":"p","request_id":"r","task":"t","total":` + total + `,"pool_uid":"pool"}`
		if _, err := command.ParseTool("child_spawn", json.RawMessage(raw)); err == nil {
			t.Errorf("invalid total %s accepted", total)
		}
	}
}
