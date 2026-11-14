package gitlab

// Verdict is an informational CI assessment, never permission to merge.
type Verdict string

const (
	CISuccess Verdict = "success"
	CIFailed  Verdict = "failed"
	CIWaiting Verdict = "waiting"
	CIBlocked Verdict = "blocked"
)

// EvidenceReason is a stable machine-readable reason; callers need not parse
// GitLab's prose diagnostics to distinguish these outcomes.
type EvidenceReason string

const (
	Complete       EvidenceReason = "complete"
	Incomplete     EvidenceReason = "incomplete"
	Inconsistent   EvidenceReason = "inconsistent"
	JobsFailed     EvidenceReason = "jobs_failed"
	PipelineFailed EvidenceReason = "pipeline_failed"
	InProgress     EvidenceReason = "in_progress"
	Unsettled      EvidenceReason = "unsettled"
)

// CIEvidence preserves identities and GitLab diagnostics apart from the
// verdict. IDs retain input order. It does not establish MR head freshness,
// approvals, mergeability, grants or lifecycle authority.
type CIEvidence struct {
	Verdict              Verdict        `json:"verdict"`
	Reason               EvidenceReason `json:"reason"`
	ProjectID            int64          `json:"project_id"`
	PipelineID           int64          `json:"pipeline_id"`
	SHA                  string         `json:"sha"`
	PipelineStatus       string         `json:"pipeline_status"`
	FailedJobIDs         []int64        `json:"failed_job_ids"`
	AllowedFailureJobIDs []int64        `json:"allowed_failure_job_ids"`
	BlockingJobIDs       []int64        `json:"blocking_job_ids"`
	YAMLErrors           string         `json:"yaml_errors"`
	FailureReason        string         `json:"failure_reason"`
	DetailedStatus       string         `json:"detailed_status"`
}

// AssessCI is pure and conservative. Like brood/dev/gitlab.py:is_failure,
// any failed job without allow_failure fails even a running pipeline. Allowed
// failures are separate evidence, not the cause of failure. Pipeline-level
// diagnostics remain available when GitLab rejects YAML without making jobs.
//
// Only a success pipeline with nonempty, consistent current jobs, all success
// or allowed failures, is CISuccess. Unknown/manual/skipped/canceled states
// are blocked. Successful evidence is not merge authorization. Callers must
// use complete reads: arbitrary slices cannot prove pagination completeness.
func AssessCI(p Pipeline, jobs []Job) CIEvidence {
	e := CIEvidence{
		Verdict: CIBlocked, Reason: Incomplete,
		ProjectID: p.ProjectID, PipelineID: p.ID, SHA: p.SHA, PipelineStatus: p.Status,
		YAMLErrors: p.YAMLErrors, FailureReason: p.FailureReason, DetailedStatus: p.DetailedStatus.Text,
	}
	if p.ID <= 0 || p.ProjectID <= 0 || p.SHA == "" {
		return e
	}
	seen := make(map[int64]bool)
	for _, j := range jobs {
		if j.ID <= 0 || seen[j.ID] || j.Pipeline.ID != p.ID || j.Pipeline.ProjectID != p.ProjectID || j.Pipeline.SHA != p.SHA {
			e.Reason = Inconsistent
			return e
		}
		seen[j.ID] = true
	}
	unknown := false
	for _, j := range jobs {
		switch {
		case j.Status == "failed" && !j.AllowFailure:
			e.FailedJobIDs = append(e.FailedJobIDs, j.ID)
		case j.Status == "failed" && j.AllowFailure:
			e.AllowedFailureJobIDs = append(e.AllowedFailureJobIDs, j.ID)
		case j.Status == "success":
		default:
			e.BlockingJobIDs = append(e.BlockingJobIDs, j.ID)
			if !inProgress(j.Status) {
				unknown = true
			}
		}
	}
	switch {
	case len(e.FailedJobIDs) > 0:
		e.Verdict = CIFailed
		e.Reason = JobsFailed
	case p.Status == "failed" || p.YAMLErrors != "":
		e.Verdict = CIFailed
		e.Reason = PipelineFailed
	case unknown || p.Status != "success" && !inProgress(p.Status):
		e.Reason = Unsettled
	case inProgress(p.Status) || len(e.BlockingJobIDs) > 0:
		e.Verdict = CIWaiting
		e.Reason = InProgress
	case len(jobs) == 0:
		e.Reason = Incomplete
	default:
		e.Verdict = CISuccess
		e.Reason = Complete
	}
	return e
}

func inProgress(status string) bool {
	switch status {
	case "created", "waiting_for_resource", "preparing", "pending", "running", "waiting_for_callback":
		return true
	}
	return false
}
