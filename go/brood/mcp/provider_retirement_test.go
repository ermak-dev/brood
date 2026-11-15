package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
)

// FEATURE: Старые клиенты не вызывают удалённые интеграции платформы (#304).
// SPEC: В MCP нет GitLab инструментов; вызовы отвергаются до executor.
func TestRetiredProviderToolsNeverExecute(t *testing.T) {
	names := []string{
		"gitlab_issue_get", "gitlab_ci_get", "gitlab_mr_create",
		"gitlab_operation_get", "gitlab_merge_propose", "gitlab_proposal_get",
	}
	args := []string{
		`{"iid":7}`, `{"mr_iid":7}`,
		`{"request_id":"r1","source_branch":"work","title":"Work","expected_source_sha":"` + strings.Repeat("a", 40) + `"}`,
		`{"operation_id":"op"}`,
		`{"request_id":"r2","mr_iid":7,"expected_source_sha":"` + strings.Repeat("a", 40) + `","pipeline_id":1}`,
		`{"operation_id":"op"}`,
	}
	input := handshake + "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n"
	for n, name := range names {
		input += fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"tools/call\",\"params\":{\"name\":%q,\"arguments\":%s}}\n", n+3, name, args[n])
	}
	var output bytes.Buffer
	calls := 0
	err := Serve(context.Background(), strings.NewReader(input), &output, executorFunc(func(context.Context, command.Invocation) (command.Result, error) {
		calls++
		return command.Result{}, nil
	}), command.Specs())
	if err != nil {
		t.Fatal(err)
	}
	frames := responses(t, &output)
	if len(frames) != len(names)+2 {
		t.Fatalf("got %d responses, want %d", len(frames), len(names)+2)
	}
	tools := frames[1]["result"].(map[string]any)["tools"].([]any)
	for _, tool := range tools {
		name := tool.(map[string]any)["name"].(string)
		if strings.HasPrefix(name, "gitlab_") {
			t.Errorf("retired provider tool advertised: %s", name)
		}
	}
	for n, frame := range frames[2:] {
		if frame["error"] == nil {
			raw, _ := json.Marshal(frame)
			t.Errorf("retired %s accepted: %s", names[n], raw)
		}
	}
	if calls != 0 {
		t.Fatalf("retired tools reached executor %d times", calls)
	}
}
