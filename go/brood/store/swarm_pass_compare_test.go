package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Сравнение трасс (#293) сопоставляет записанные позиции, не угадывает
// семантическое соответствие узлов и не превращает reported result в actual.
func comparisonTrace(id string, pass int64, nodes ...string) SwarmPassTrace {
	trace := SwarmPassTrace{Version: 1, SwarmID: id, Pass: pass, Goal: "same goal", Origin: "https://git.example.org/team/repo.git", SourceCommit: strings.Repeat("a", 40), DefinitionName: "deliver", DefinitionVersion: 1, DefinitionRevision: "v1", Steps: []SwarmPassTraceStep{}}
	for index, node := range nodes {
		outcome := "success"
		trace.Steps = append(trace.Steps, SwarmPassTraceStep{Visit: PipelineVisit{Sequence: pass + int64(index), Pass: pass, NodeID: node, State: "completed", Outcome: &outcome, Report: "retained report"}})
	}
	return trace
}

func TestCompareSwarmPassTracesUsesPositionNotGlobalSequence(t *testing.T) {
	reference := comparisonTrace("reference", 1, "build", "review")
	candidate := comparisonTrace("candidate", 17, "build", "review")
	candidate.DefinitionRevision = "v2"
	candidate.DefinitionVersion = 2
	fail := "fail"
	candidate.Steps[1].Visit.Outcome = &fail
	candidate.Steps[1].Visit.Report = "different retained report"
	beforeReference, _ := json.Marshal(reference)
	beforeCandidate, _ := json.Marshal(candidate)
	comparison, err := CompareSwarmPassTraces(reference, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Version != 1 || comparison.Alignment != "visit-position" || comparison.Reference.SwarmID != "reference" || comparison.Reference.Pass != 1 || comparison.Candidate.Pass != 17 {
		t.Fatalf("wrong comparison identity: %+v", comparison)
	}
	if !comparison.SameGoal || !comparison.SameSource || comparison.SameDefinition || comparison.StepCount.Reference != 2 || comparison.StepCount.Candidate != 2 || comparison.StepCount.Delta != 0 {
		t.Fatalf("wrong context/counts: %+v", comparison)
	}
	if len(comparison.Steps) != 2 || comparison.Steps[0].Position != 1 || comparison.Steps[0].Reference.Sequence != 1 || comparison.Steps[0].Candidate.Sequence != 17 || comparison.Steps[0].Outcome != "same" || comparison.Steps[1].Outcome != "different" || comparison.Steps[1].Report != "different" {
		t.Fatalf("wrong position/report comparison: %+v", comparison.Steps)
	}
	afterReference, _ := json.Marshal(reference)
	afterCandidate, _ := json.Marshal(candidate)
	if string(beforeReference) != string(afterReference) || string(beforeCandidate) != string(afterCandidate) {
		t.Fatal("comparison mutated its source traces")
	}
	*comparison.Steps[0].Reference.Outcome = "changed output"
	if *reference.Steps[0].Visit.Outcome != "success" {
		t.Fatal("comparison aliases source outcome pointers")
	}
}

func TestCompareSwarmPassTracesNeverAlignsDifferentNodesOrInventsMissingResults(t *testing.T) {
	reference := comparisonTrace("reference", 1, "build", "review", "finish")
	candidate := comparisonTrace("candidate", 10, "build", "fix")
	reference.Steps[0].Visit.Outcome = nil
	reference.Steps[0].Visit.State = "inflight"
	reference.Steps[0].Completion = &PipelineCompletionIntent{ActorCompletion: ActorCompletion{Outcome: "success", Report: "reported, not actual"}}
	candidate.Steps[0].Visit.Outcome = nil
	candidate.Steps[0].Visit.State = "inflight"
	candidate.Steps[0].Completion = &PipelineCompletionIntent{ActorCompletion: ActorCompletion{Outcome: "fail", Report: "different report"}}
	reference.Steps[1].Children = &SwarmChildJoin{Children: []SwarmChildTask{{}, {}}}
	candidate.Steps[1].Children = &SwarmChildJoin{Children: []SwarmChildTask{{}}}
	comparison, err := CompareSwarmPassTraces(reference, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.StepCount.Delta != -1 || comparison.ChildTaskCount.Reference != 2 || comparison.ChildTaskCount.Candidate != 1 || comparison.ChildTaskCount.Delta != -1 {
		t.Fatal("missing steps/child task counts were hidden")
	}
	first := comparison.Steps[0]
	if first.Outcome != "unknown" || first.Report != "unknown" || first.ReportedOutcome != "different" || first.ReportedReport != "different" || first.Reference.Outcome != nil || first.Reference.ReportedOutcome == nil || *first.Reference.ReportedOutcome != "success" {
		t.Fatalf("reported work became actual or disappeared: %+v", first)
	}
	if comparison.Steps[1].Node != "different" || comparison.Steps[1].Outcome != "not_comparable" || comparison.Steps[1].Report != "not_comparable" {
		t.Fatal("same strings at different graph nodes were compared as equivalent")
	}
	last := comparison.Steps[2]
	if last.Reference == nil || last.Candidate != nil || last.Node != "missing" || last.Outcome != "not_comparable" {
		t.Fatal("missing trailing visit was silently truncated")
	}
}

func TestCompareSwarmPassTracesRetainsOnlyRealNonnegativeDurations(t *testing.T) {
	reference := comparisonTrace("reference", 1, "a", "b", "c", "d")
	candidate := comparisonTrace("candidate", 10, "a", "b", "c", "d")
	set := func(step *SwarmPassTraceStep, start, finish string) {
		step.Visit.StartedAt = &start
		step.Visit.FinishedAt = &finish
	}
	set(&reference.Steps[0], "2026-11-17T00:00:00Z", "2026-11-17T00:00:01.500Z")
	set(&candidate.Steps[0], "2026-11-17T01:00:00+01:00", "2026-11-17T01:00:02+01:00")
	set(&reference.Steps[1], "invalid", "2026-11-17T00:00:02Z")
	set(&candidate.Steps[1], "2026-11-17T00:00:00Z", "2026-11-17T00:00:02Z")
	set(&reference.Steps[2], "2026-11-17T00:00:02Z", "2026-11-17T00:00:01Z")
	set(&candidate.Steps[2], "2026-11-17T00:00:00Z", "2026-11-17T00:00:02Z")
	set(&reference.Steps[3], "0001-01-01T00:00:00Z", "9999-01-01T00:00:00Z")
	comparison, err := CompareSwarmPassTraces(reference, candidate)
	if err != nil {
		t.Fatal(err)
	}
	first := comparison.Steps[0]
	if first.Reference.DurationMillis == nil || *first.Reference.DurationMillis != 1500 || first.Candidate.DurationMillis == nil || *first.Candidate.DurationMillis != 2000 || first.DurationDeltaMillis == nil || *first.DurationDeltaMillis != 500 {
		t.Fatalf("exact recorded elapsed time lost: %+v", first)
	}
	for _, step := range comparison.Steps[1:] {
		if step.Reference.DurationMillis != nil || step.DurationDeltaMillis != nil {
			t.Fatal("missing, malformed, negative or overflowing duration was invented")
		}
	}
}

func TestCompareSwarmPassTracesRejectsInvalidOrOversizedInputsWithoutPartialReport(t *testing.T) {
	good := comparisonTrace("reference", 1, "build")
	for _, change := range []func(*SwarmPassTrace){
		func(v *SwarmPassTrace) { v.Version = 2 },
		func(v *SwarmPassTrace) { v.Pass = 0 },
		func(v *SwarmPassTrace) { v.SwarmID = "" },
		func(v *SwarmPassTrace) { v.Steps = nil },
		func(v *SwarmPassTrace) { v.Steps[0].Visit.Sequence = 2 },
		func(v *SwarmPassTrace) { v.Steps = append(v.Steps, v.Steps[0]) },
		func(v *SwarmPassTrace) { v.Steps[0].Visit.NodeID = "" },
		func(v *SwarmPassTrace) { v.Steps = make([]SwarmPassTraceStep, MaxPassTraceRecords+1) },
		func(v *SwarmPassTrace) { v.Steps[0].Visit.Report = strings.Repeat("x", MaxPassTraceBytes+1) },
	} {
		invalid := comparisonTrace("candidate", 1, "build")
		change(&invalid)
		got, err := CompareSwarmPassTraces(good, invalid)
		if err == nil || !reflect.DeepEqual(got, SwarmPassComparison{}) {
			t.Fatal("invalid input returned partial comparison or success")
		}
		got, err = CompareSwarmPassTraces(invalid, good)
		if err == nil || !reflect.DeepEqual(got, SwarmPassComparison{}) {
			t.Fatal("invalid reference returned partial comparison or success")
		}
	}
}

func TestCompareSwarmPassTracesBudgetIncludesJSONSeparators(t *testing.T) {
	reference := comparisonTrace("reference", 1, "build", "review")
	candidate := comparisonTrace("candidate", 10, "build", "review")
	for index := range reference.Steps {
		reference.Steps[index].Visit.Report = ""
		candidate.Steps[index].Visit.Report = ""
	}
	reference.Steps[0].Visit.Report = "x"
	base, err := CompareSwarmPassTraces(reference, candidate)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	reference.Steps[0].Visit.Report = strings.Repeat("x", MaxPassTraceBytes-len(data)+2)
	got, err := CompareSwarmPassTraces(reference, candidate)
	if err == nil || !reflect.DeepEqual(got, SwarmPassComparison{}) {
		t.Fatal("JSON separators allowed an oversized comparison document")
	}
	reference.Steps[0].Visit.Report = strings.Repeat("x", MaxPassTraceBytes-len(data)+1)
	got, err = CompareSwarmPassTraces(reference, candidate)
	if err != nil {
		t.Fatal("exactly bounded comparison refused:", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || len(encoded) != MaxPassTraceBytes {
		t.Fatal("comparison size boundary was not exact")
	}
}

func TestCompareSwarmPassTracesDoesNotExposeAuthorityOrClaimEqualSources(t *testing.T) {
	reference := comparisonTrace("reference", 1, "build")
	candidate := comparisonTrace("candidate", 1, "build")
	candidate.Goal = "another goal"
	candidate.Origin = "https://git.example.org/other/repo.git"
	candidate.SourceCommit = strings.Repeat("b", 40)
	reference.Steps[0].ResultObservation = &PipelineResultObservation{Lease: ActorOperation{OperationID: "private-root-lease"}}
	comparison, err := CompareSwarmPassTraces(reference, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.SameGoal || comparison.SameSource {
		t.Fatal("different comparison context hidden")
	}
	data, err := json.Marshal(comparison)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-root-lease") || strings.Contains(string(data), "Lease") || strings.Contains(string(data), "ResultObservation") {
		t.Fatal("comparison serialized root authority material")
	}
}
