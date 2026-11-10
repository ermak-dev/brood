package command_test

import (
	"encoding/json"
	"strings"
	"testing"

	"brood.dev/go/brood/command"
)

// FEATURE: The actor reports what a step did together with its outcome (#233).
// SPEC: pipeline_visit_complete takes an optional report of at most 16384
// characters; without it the canonical arguments carry no report. The CLI
// spells it --report. Identity fields stay unknown arguments.
func TestPipelineVisitCompleteReport(t *testing.T) {
	s, ok := command.Lookup("pipeline_visit_complete")
	if !ok || !s.Guest {
		t.Fatalf("pipeline_visit_complete metadata: %+v %v", s, ok)
	}
	properties := s.Schema()["properties"].(map[string]any)
	report, ok := properties["report"].(map[string]any)
	if !ok || report["type"] != "string" || report["maxLength"] != 16384 || report["description"] == nil {
		t.Fatalf("report schema: %v", properties["report"])
	}
	for _, required := range s.Schema()["required"].([]string) {
		if required == "report" {
			t.Fatal("report is required")
		}
	}
	base := `{"run_id":"run1","sequence":1,"request_id":"req1","outcome":"success"`
	plain, err := command.ParseTool("pipeline_visit_complete", json.RawMessage(base+`}`))
	if err != nil || strings.Contains(string(plain.Arguments), "report") {
		t.Fatalf("absent report: %s %v", plain.Arguments, err)
	}
	reported, err := command.ParseTool("pipeline_visit_complete", json.RawMessage(base+`,"report":"Branch feat/1-x, merge request !3."}`))
	var args map[string]any
	if err != nil || json.Unmarshal(reported.Arguments, &args) != nil || args["report"] != "Branch feat/1-x, merge request !3." {
		t.Fatalf("report: %s %v", reported.Arguments, err)
	}
	oversized, _ := json.Marshal(strings.Repeat("x", 16385))
	if _, err := command.ParseTool("pipeline_visit_complete", json.RawMessage(base+`,"report":`+string(oversized)+`}`)); err == nil {
		t.Fatal("oversized report accepted")
	}
	for _, forged := range []string{"actor_uid", "incarnation", "swarm_id"} {
		if _, err := command.ParseTool("pipeline_visit_complete", json.RawMessage(base+`,"`+forged+`":"x"}`)); err == nil {
			t.Errorf("%s accepted", forged)
		}
	}
	cli, err := command.ParseCLI([]string{"pipeline", "visit", "complete", "--run-id", "run1", "--sequence", "1", "--request-id", "req1", "--outcome", "success", "--report", "Done on branch feat/1-x."})
	if err != nil || cli.Name != "pipeline_visit_complete" || !strings.Contains(string(cli.Arguments), `"report":"Done on branch feat/1-x."`) {
		t.Fatalf("cli: %+v %v", cli, err)
	}
}
