//go:build linux

package guest

import (
	"encoding/json"
	"testing"
)

// FEATURE: Agents use the brood command adapter.
// SPEC: Explicit guest mode, additive brood namespace, no supervisor binary.
// The session keeps the actor's own MCP configs.
func TestClaudeReceivesAdditiveCredentialFreeMCPConfig(t *testing.T) {
	args := projectAppArgs()
	for _, arg := range args {
		if arg == "--strict-mcp-config" || arg == "--setting-sources" {
			t.Fatal("existing MCP configs and settings must be preserved")
		}
	}
	assertBroodOnlyMCPConfig(t, args)
}

func assertBroodOnlyMCPConfig(t *testing.T, args []string) {
	t.Helper()
	var value string
	for i, arg := range args {
		if arg == "--mcp-config" && i+1 < len(args) {
			value = args[i+1]
		}
	}
	if value == "" {
		t.Fatal("managed Claude is missing brood MCP configuration")
	}
	var config struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(value), &config) != nil {
		t.Fatal("invalid MCP config")
	}
	server := config.MCPServers["brood"]
	if len(config.MCPServers) != 1 {
		t.Fatalf("unexpected MCP namespaces: %v", config.MCPServers)
	}
	if server.Command != "/opt/brood/bin/bro" || len(server.Args) != 2 || server.Args[0] != "--guest" || server.Args[1] != "mcp" {
		t.Fatalf("server: %+v", server)
	}
}
