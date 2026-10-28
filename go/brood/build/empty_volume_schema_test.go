package build

import (
	"strings"
	"testing"
)

// BUG: maxProperties=0 is confused with an absent limit, accepting unknown durableDir fields.
// HYPOTHESIS: The schema node stores an optional zero bound as a plain int (#22).
// SOLUTION: A pointer preserves an explicitly configured zero without limiting other maps.
// RESULT: FIXED — parser, plan and actual CLI reject unknown settings before Docker.
func TestDurableDirectoryRejectsUnknownFieldsBeforeBuild(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		document := minimal(t)
		settings := map[string]any{}
		if invalid {
			settings["unsupported"] = "secret-value"
		}
		replace(document, []any{"templates", "python", "actor", "volumes"}, []any{
			map[string]any{"name": "workspace", "durableDir": settings},
		})
		_, parseErr := ParseDocument(marshal(t, document), "Broodfile")
		_, planErr := MakePlan(document, nil, "", "localhost:5002", "swarm", "Broodfile")
		for name, err := range map[string]error{"parse": parseErr, "plan": planErr} {
			if !invalid {
				if err != nil {
					t.Errorf("%s rejected empty durableDir: %v", name, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s accepted unknown durableDir fields", name)
				continue
			}
			if !strings.Contains(err.Error(), "durableDir") || !strings.Contains(err.Error(), "maxProperties") {
				t.Errorf("%s error lacks schema field/reason: %v", name, err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Errorf("%s error disclosed a rejected value", name)
			}
		}
	}
}
