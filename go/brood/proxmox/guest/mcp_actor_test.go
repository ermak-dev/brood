//go:build linux

package guest

import (
	"encoding/json"
	"testing"
)

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
	server := config.MCPServers["mop"]
	if len(config.MCPServers) != 1 {
		t.Fatalf("unexpected MCP namespaces: %v", config.MCPServers)
	}
	if server.Command != "/usr/local/bin/brood-guest" || len(server.Args) != 1 || server.Args[0] != "mcp" {
		t.Fatalf("server: %+v", server)
	}
}
