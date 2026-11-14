package gitlab

// FEATURE: Pure, fail-closed CI evidence from pipeline and current jobs.
// SPEC: A non-allow_failure failed job wins even while the pipeline is running;
// manual/skipped/unknown or incomplete/mismatched evidence is never success.
// IMPLEMENTATION: Validate identities first, then separate failed, tolerated
// and blocking jobs; preserve pipeline diagnostics independently of verdict.
// RESULT: IMPLEMENTED - Pure table cases preserve bindings and fail closed.
// STATUS: DONE

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCIEvidenceCarriesProjectAndCommit(t *testing.T) {
	p := Pipeline{ID: 90, ProjectID: 42, SHA: "abc", Status: "success"}
	e := AssessCI(p, []Job{{ID: 1, Status: "success", Pipeline: p}})
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["project_id"] != float64(42) || wire["sha"] != "abc" {
		t.Fatalf("evidence loses project/commit binding: %s", b)
	}
}

func TestCIEvidence(t *testing.T) {
	p := Pipeline{ID: 90, ProjectID: 42, SHA: "abc", Status: "success"}
	job := Job{ID: 1, Status: "success", Pipeline: Pipeline{ID: 90, ProjectID: 42, SHA: "abc"}}
	tests := []struct {
		name                     string
		status                   string
		jobs                     []Job
		verdict                  Verdict
		reason                   EvidenceReason
		failed, allowed, blocked []int64
	}{
		{name: "success", status: "success", jobs: []Job{job}, verdict: CISuccess, reason: Complete},
		{name: "no jobs", status: "success", verdict: CIBlocked, reason: Incomplete},
		{name: "pipeline failed without jobs", status: "failed", verdict: CIFailed, reason: PipelineFailed},
		{name: "running job", status: "running", jobs: []Job{{ID: 1, Status: "running", Pipeline: job.Pipeline}}, verdict: CIWaiting, reason: InProgress, blocked: []int64{1}},
		{name: "failed while running", status: "running", jobs: []Job{{ID: 1, Status: "failed", Pipeline: job.Pipeline}}, verdict: CIFailed, reason: JobsFailed, failed: []int64{1}},
		{name: "allowed failure", status: "success", jobs: []Job{{ID: 1, Status: "failed", AllowFailure: true, Pipeline: job.Pipeline}}, verdict: CISuccess, reason: Complete, allowed: []int64{1}},
		{name: "manual pipeline", status: "manual", jobs: []Job{job}, verdict: CIBlocked, reason: Unsettled},
		{name: "skipped pipeline", status: "skipped", jobs: []Job{job}, verdict: CIBlocked, reason: Unsettled},
		{name: "unknown pipeline", status: "future_status", jobs: []Job{job}, verdict: CIBlocked, reason: Unsettled},
		{name: "manual job", status: "success", jobs: []Job{{ID: 1, Status: "manual", AllowFailure: true, Pipeline: job.Pipeline}}, verdict: CIBlocked, reason: Unsettled, blocked: []int64{1}},
		{name: "skipped job", status: "success", jobs: []Job{{ID: 1, Status: "skipped", Pipeline: job.Pipeline}}, verdict: CIBlocked, reason: Unsettled, blocked: []int64{1}},
		{name: "unknown job", status: "success", jobs: []Job{{ID: 1, Status: "future_status", Pipeline: job.Pipeline}}, verdict: CIBlocked, reason: Unsettled, blocked: []int64{1}},
		{name: "canceled pipeline", status: "canceled", jobs: []Job{job}, verdict: CIBlocked, reason: Unsettled},
		{name: "foreign project", status: "success", jobs: []Job{{ID: 1, Status: "success", Pipeline: Pipeline{ID: 90, ProjectID: 43, SHA: "abc"}}}, verdict: CIBlocked, reason: Inconsistent},
		{name: "foreign pipeline", status: "success", jobs: []Job{{ID: 1, Status: "success", Pipeline: Pipeline{ID: 91, ProjectID: 42, SHA: "abc"}}}, verdict: CIBlocked, reason: Inconsistent},
		{name: "foreign sha", status: "success", jobs: []Job{{ID: 1, Status: "success", Pipeline: Pipeline{ID: 90, ProjectID: 42, SHA: "def"}}}, verdict: CIBlocked, reason: Inconsistent},
		{name: "duplicate job", status: "success", jobs: []Job{job, job}, verdict: CIBlocked, reason: Inconsistent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p.Status = tt.status
			before := append([]Job(nil), tt.jobs...)
			got := AssessCI(p, tt.jobs)
			if got.Verdict != tt.verdict || got.Reason != tt.reason || got.PipelineID != 90 || got.PipelineStatus != tt.status || !reflect.DeepEqual(got.FailedJobIDs, tt.failed) || !reflect.DeepEqual(got.AllowedFailureJobIDs, tt.allowed) || !reflect.DeepEqual(got.BlockingJobIDs, tt.blocked) {
				t.Fatalf("evidence: %+v; want %s/%s failed=%v allowed=%v blocked=%v", got, tt.verdict, tt.reason, tt.failed, tt.allowed, tt.blocked)
			}
			if !reflect.DeepEqual(before, tt.jobs) {
				t.Fatal("mutated input jobs")
			}
		})
	}
}

func TestCIPipelineReasonsAndConservativeInputs(t *testing.T) {
	p := Pipeline{ID: 90, ProjectID: 42, SHA: "abc", Status: "failed", YAMLErrors: "bad yaml", FailureReason: "config_error", DetailedStatus: DetailedStatus{Text: "configuration invalid"}}
	e := AssessCI(p, nil)
	if e.Verdict != CIFailed || e.YAMLErrors != p.YAMLErrors || e.FailureReason != p.FailureReason || e.DetailedStatus != p.DetailedStatus.Text {
		t.Fatalf("lost structured pipeline reason: %+v", e)
	}
	p.Status = "success"
	if e = AssessCI(p, []Job{{ID: 1, Status: "success", Pipeline: p}}); e.Verdict != CIFailed {
		t.Fatalf("yaml error accepted: %+v", e)
	}
	for _, p := range []Pipeline{{}, {ID: 90, ProjectID: 42, Status: "success"}, {ID: 90, ProjectID: 0, SHA: "abc", Status: "success"}} {
		if e := AssessCI(p, nil); e.Verdict != CIBlocked {
			t.Fatalf("incomplete pipeline accepted: %+v", e)
		}
	}
	for _, s := range []string{"created", "waiting_for_resource", "preparing", "pending", "running", "waiting_for_callback"} {
		p := Pipeline{ID: 90, ProjectID: 42, SHA: "abc", Status: s}
		if e := AssessCI(p, nil); e.Verdict != CIWaiting {
			t.Fatalf("running state %s: %+v", s, e)
		}
	}
}
