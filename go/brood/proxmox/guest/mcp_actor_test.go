//go:build linux

package guest

import (
	"encoding/json"
	"slices"
	"testing"
)

// FEATURE: Managed actors use the brood command adapter.
// SPEC: Explicit guest mode, additive brood namespace, no supervisor binary.
// A free session keeps the actor's own MCP configs; a step session (#224)
// has brood alone.
func TestClaudeReceivesAdditiveCredentialFreeMCPConfig(t *testing.T) {
	args := projectAppArgs()
	for _, arg := range args {
		if arg == "--strict-mcp-config" || arg == "--setting-sources" {
			t.Fatal("existing MCP configs and settings must be preserved")
		}
	}
	assertBroodOnlyMCPConfig(t, args)
}

func TestStepClaudeUsesOnlyBroodMCPAndProjectSettings(t *testing.T) {
	args := stepAppArgs()
	if args[0] != "claude" || !slices.Contains(args, "--dangerously-skip-permissions") || !slices.Contains(args, "--strict-mcp-config") {
		t.Fatalf("step session argv: %q", args)
	}
	// Пользовательские и локальные настройки (их env, statusLine, plugins) не
	// читаются; проектные — часть подтверждённого commit, с CLAUDE.md.
	if at := slices.Index(args, "--setting-sources"); at < 0 || at+1 >= len(args) || args[at+1] != "project" {
		t.Fatalf("step session setting sources: %q", args)
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
