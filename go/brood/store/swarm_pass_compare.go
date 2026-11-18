package store

import (
	"errors"
	"time"
)

// ErrInvalidPassComparison identifies incomplete or inconsistent comparison inputs.
var ErrInvalidPassComparison = errors.New("pass comparison requires valid ordered version 1 traces")

// SwarmPassComparison describes retained differences, not semantic equivalence,
// eval provenance or authority to execute either pass. Steps align by recorded
// position; different node IDs are explicitly not comparable at that position.
type SwarmPassComparison struct {
	Version        int                       `json:"version"`
	Alignment      string                    `json:"alignment"`
	Reference      SwarmPassComparisonRef    `json:"reference"`
	Candidate      SwarmPassComparisonRef    `json:"candidate"`
	SameGoal       bool                      `json:"sameGoal"`
	SameSource     bool                      `json:"sameSource"`
	SameDefinition bool                      `json:"sameDefinition"`
	StepCount      SwarmPassCountComparison  `json:"stepCount"`
	ChildTaskCount SwarmPassCountComparison  `json:"childTaskCount"`
	Steps          []SwarmPassStepComparison `json:"steps"`
}

// SwarmPassComparisonRef identifies one selected pass, not its current execution.
type SwarmPassComparisonRef struct {
	SwarmID string `json:"swarmId"`
	Pass    int64  `json:"pass"`
}

// SwarmPassCountComparison counts retained records. Delta is candidate-reference.
type SwarmPassCountComparison struct {
	Reference int `json:"reference"`
	Candidate int `json:"candidate"`
	Delta     int `json:"delta"`
}

// SwarmPassStepComparison keeps actual and reported differences separate.
// Statuses are same, different, unknown or not_comparable. Node alone may be
// missing when only one side has a visit at the recorded position.
type SwarmPassStepComparison struct {
	Position            int                    `json:"position"`
	Reference           *SwarmPassComparedStep `json:"reference,omitempty"`
	Candidate           *SwarmPassComparedStep `json:"candidate,omitempty"`
	Node                string                 `json:"node"`
	Outcome             string                 `json:"outcome"`
	Report              string                 `json:"report"`
	ReportedOutcome     string                 `json:"reportedOutcome"`
	ReportedReport      string                 `json:"reportedReport"`
	DurationDeltaMillis *int64                 `json:"durationDeltaMillis,omitempty"`
}

// SwarmPassComparedStep projects only comparison material. An absent actual
// outcome/report is unknown; a retained completion does not fill that absence.
// Durations are elapsed wall-clock time between valid saved timestamps, not
// model latency. Missing, reversed or overflowing intervals stay absent.
type SwarmPassComparedStep struct {
	Sequence        int64   `json:"sequence"`
	NodeID          string  `json:"nodeId"`
	State           string  `json:"state"`
	Outcome         *string `json:"outcome,omitempty"`
	Report          *string `json:"report,omitempty"`
	ReportedOutcome *string `json:"reportedOutcome,omitempty"`
	ReportedReport  *string `json:"reportedReport,omitempty"`
	DurationMillis  *int64  `json:"durationMillis,omitempty"`
	ChildTasks      int     `json:"childTasks"`
}

// CompareSwarmPassTraces is pure. Callers obtain consistent traces and enforce
// visibility of both sources independently; this function neither reads a
// database nor authorizes replay, landing, observation or any actor operation.
// Invalid or oversized material returns no partial report. The output shares
// no mutable pointers with either input and never includes root authority data.
func CompareSwarmPassTraces(reference, candidate SwarmPassTrace) (SwarmPassComparison, error) {
	if !validComparisonTrace(reference) || !validComparisonTrace(candidate) {
		return SwarmPassComparison{}, ErrInvalidPassComparison
	}
	result := SwarmPassComparison{
		Version: 1, Alignment: "visit-position",
		Reference:      SwarmPassComparisonRef{SwarmID: reference.SwarmID, Pass: reference.Pass},
		Candidate:      SwarmPassComparisonRef{SwarmID: candidate.SwarmID, Pass: candidate.Pass},
		SameGoal:       reference.Goal != "" && reference.Goal == candidate.Goal,
		SameSource:     reference.Origin != "" && reference.SourceCommit != "" && reference.Origin == candidate.Origin && reference.SourceCommit == candidate.SourceCommit,
		SameDefinition: reference.DefinitionName != "" && reference.DefinitionRevision != "" && reference.DefinitionVersion > 0 && reference.DefinitionName == candidate.DefinitionName && reference.DefinitionVersion == candidate.DefinitionVersion && reference.DefinitionRevision == candidate.DefinitionRevision,
		StepCount:      countComparison(len(reference.Steps), len(candidate.Steps)),
		ChildTaskCount: countComparison(traceChildTaskCount(reference), traceChildTaskCount(candidate)),
		Steps:          []SwarmPassStepComparison{},
	}
	budget := &passTraceBudget{}
	if err := budget.add(result); err != nil {
		return SwarmPassComparison{}, err
	}
	for position := 0; position < max(len(reference.Steps), len(candidate.Steps)); position++ {
		row := SwarmPassStepComparison{Position: position + 1, Node: "missing", Outcome: "not_comparable", Report: "not_comparable", ReportedOutcome: "not_comparable", ReportedReport: "not_comparable"}
		if position < len(reference.Steps) {
			row.Reference = comparedStep(reference.Steps[position])
		}
		if position < len(candidate.Steps) {
			row.Candidate = comparedStep(candidate.Steps[position])
		}
		if row.Reference != nil && row.Candidate != nil {
			row.Node = "different"
			if row.Reference.NodeID == row.Candidate.NodeID {
				row.Node = "same"
				row.Outcome = compareSavedText(row.Reference.Outcome, row.Candidate.Outcome)
				row.Report = compareSavedText(row.Reference.Report, row.Candidate.Report)
				row.ReportedOutcome = compareSavedText(row.Reference.ReportedOutcome, row.Candidate.ReportedOutcome)
				row.ReportedReport = compareSavedText(row.Reference.ReportedReport, row.Candidate.ReportedReport)
				if row.Reference.DurationMillis != nil && row.Candidate.DurationMillis != nil {
					delta := *row.Candidate.DurationMillis - *row.Reference.DurationMillis
					row.DurationDeltaMillis = &delta
				}
			}
		}
		if position > 0 {
			budget.used++
		}
		if err := budget.add(row); err != nil {
			return SwarmPassComparison{}, err
		}
		result.Steps = append(result.Steps, row)
	}
	return result, nil
}

func validComparisonTrace(trace SwarmPassTrace) bool {
	if trace.Version != 1 || !validPipelineField(trace.SwarmID) || trace.Pass < 1 || len(trace.Steps) == 0 || len(trace.Steps) > MaxPassTraceRecords {
		return false
	}
	previous := trace.Pass - 1
	for index, step := range trace.Steps {
		if !validPipelineField(step.Visit.NodeID) || step.Visit.Sequence <= previous || index == 0 && step.Visit.Sequence != trace.Pass {
			return false
		}
		if step.Children != nil && len(step.Children.Children) > MaxSwarmChildren {
			return false
		}
		previous = step.Visit.Sequence
	}
	return true
}

func traceChildTaskCount(trace SwarmPassTrace) int {
	count := 0
	for _, step := range trace.Steps {
		if step.Children != nil {
			count += len(step.Children.Children)
		}
	}
	return count
}

func countComparison(reference, candidate int) SwarmPassCountComparison {
	return SwarmPassCountComparison{Reference: reference, Candidate: candidate, Delta: candidate - reference}
}

func comparedStep(step SwarmPassTraceStep) *SwarmPassComparedStep {
	visit := step.Visit
	result := &SwarmPassComparedStep{Sequence: visit.Sequence, NodeID: visit.NodeID, State: visit.State, DurationMillis: comparedDuration(visit.StartedAt, visit.FinishedAt)}
	if visit.Outcome != nil {
		outcome, report := *visit.Outcome, visit.Report
		result.Outcome = &outcome
		result.Report = &report
	}
	if step.Completion != nil {
		outcome, report := step.Completion.Outcome, step.Completion.Report
		result.ReportedOutcome = &outcome
		result.ReportedReport = &report
	}
	if step.Children != nil {
		result.ChildTasks = len(step.Children.Children)
	}
	return result
}

func compareSavedText(reference, candidate *string) string {
	if reference == nil || candidate == nil {
		return "unknown"
	}
	if *reference == *candidate {
		return "same"
	}
	return "different"
}

func comparedDuration(started, finished *string) *int64 {
	if started == nil || finished == nil {
		return nil
	}
	start, err := time.Parse(time.RFC3339Nano, *started)
	if err != nil {
		return nil
	}
	end, err := time.Parse(time.RFC3339Nano, *finished)
	if err != nil || end.Before(start) {
		return nil
	}
	duration := end.Sub(start)
	if !start.Add(duration).Equal(end) {
		return nil
	}
	millis := duration.Milliseconds()
	return &millis
}
