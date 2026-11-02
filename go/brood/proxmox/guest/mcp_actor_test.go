//go:build linux

package guest

import (
	"encoding/json"
	"testing"
)

// FEATURE: Managed actors use the brood command adapter.
// SPEC: Explicit guest mode, additive brood namespace, no supervisor binary.
func TestClaudeReceivesAdditiveCredentialFreeMCPConfig(t *testing.T) {
	args := actorArgs()
	var value string
	for i, arg := range args {
		if arg == "--strict-mcp-config" {
			t.Fatal("existing MCP configs must be preserved")
		}
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
	if server.Command != "/usr/local/bin/bro" || len(server.Args) != 2 || server.Args[0] != "--guest" || server.Args[1] != "mcp" {
		t.Fatalf("server: %+v", server)
	}
}
