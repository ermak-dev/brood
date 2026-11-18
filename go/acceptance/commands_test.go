package acceptance

import (
	"encoding/json"
	"strings"
	"testing"
)

// FEATURE: Installed bro utilities and MCP use the same catalog against the real API.
// SPEC: Global commands need no swarm, quota edits persist, discovery needs no model or guest broker.
func TestRealCommandCatalog(t *testing.T) {
	bin := buildBinaries(t)
	f := newInstallation(t, bin)
	f.cli("catalog-state", "admin\n"+f.passwords[0]+"\n", 0, "login", f.origin)
	out := f.cli("catalog-state", "", 0, "project", "add", "catalog-project", "--description", "CLI metadata project")
	expectCLIContains(t, out, "catalog-project")
	admin := f.browser()
	admin.login("admin", f.passwords[0])
	var projects []project
	decode(t, admin.expect("GET", "/api/projects", nil, 200), &projects)
	if len(projects) != 1 || projects[0].Name != "catalog-project" {
		t.Fatalf("CLI project did not persist: %+v", projects)
	}
	f.cli("catalog-state", "", 0, "project", "limit", "--name", "catalog-project", "--actors", "0")
	var capacity struct {
		Capacity struct {
			MaxActors *string `json:"max_actors"`
		} `json:"capacity"`
	}
	decode(t, admin.expect("GET", "/api/projects/"+projects[0].ID, nil, 200), &capacity)
	if capacity.Capacity.MaxActors == nil || *capacity.Capacity.MaxActors != "0" {
		t.Fatalf("CLI quota did not persist: %+v", capacity)
	}
	expectCLIContains(t, f.cli("catalog-state", "", 0, "node"), "managed_pools")
	expectCLIContains(t, f.cli("catalog-state", "", 0, "profile"), "admin")
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"isolated-acceptance","version":"1"}}}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	for _, tc := range []struct {
		args  []string
		tools int
	}{{[]string{"mcp"}, 34}, {[]string{"--guest", "mcp"}, 28}} {
		output := f.cli("catalog-state", input, 0, tc.args...)
		found := false
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			var frame struct {
				ID     int `json:"id"`
				Result struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				t.Fatal(err)
			}
			if frame.ID == 2 {
				found = true
				if len(frame.Result.Tools) != tc.tools {
					t.Fatalf("MCP catalog=%d want=%d", len(frame.Result.Tools), tc.tools)
				}
				counts := map[string]int{}
				for _, tool := range frame.Result.Tools {
					counts[tool.Name]++
				}
				for _, name := range []string{"child_create", "child_get", "child_join", "child_cancel", "child_complete", "pipeline_visit_complete"} {
					if counts[name] != 1 {
						t.Fatalf("MCP catalog contains %s %d times, want 1", name, counts[name])
					}
				}
				for name := range counts {
					if strings.HasPrefix(name, "gitlab_") {
						t.Fatalf("retired provider command %s remains in installed MCP catalog", name)
					}
				}
			}
		}
		if !found {
			t.Fatal("real MCP binary returned no tools/list result")
		}
		expectCLIContains(t, output, `"name":"brood"`)
	}
	f.stop()
	expectCLIContains(t, f.cli("catalog-state", "", 0, "diagnose"), "validity not checked")
}
