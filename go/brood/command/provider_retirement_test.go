package command_test

import (
	"encoding/json"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
)

// FEATURE: Проектные интеграции не являются командами платформы (#304).
// SPEC: Старые GitLab команды отсутствуют в discovery и отвергаются обоими парсерами.
func TestRetiredGitLabCommandsUnavailable(t *testing.T) {
	for _, name := range []string{
		"gitlab_issue_get", "gitlab_ci_get", "gitlab_mr_create",
		"gitlab_operation_get", "gitlab_merge_propose", "gitlab_proposal_get",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := command.Lookup(name); ok {
				t.Errorf("retired provider command %s remains advertised", name)
			}
			for _, spec := range command.Specs() {
				if spec.ToolName == name {
					t.Errorf("retired provider command %s remains in discovery", name)
				}
			}
			if _, err := command.ParseTool(name, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "unknown command") {
				t.Errorf("retired tool must be unknown, got %v", err)
			}
			if _, err := command.ParseCLI(strings.Split(name, "_")); err == nil || !strings.Contains(err.Error(), "unknown command") {
				t.Errorf("retired CLI command must be unknown, got %v", err)
			}
		})
	}
}
