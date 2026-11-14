// Package gitlab provides bounded reads and one-shot same-project MR creation
// for one explicitly bound GitLab project. It has no store, server, Git transport
// or environment access.
// CI evidence is informational: no result grants access or authorizes a merge.
package gitlab

// Project is the non-secret metadata of the bound project.
type Project struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	WebURL            string `json:"web_url"`
}

// Issue is an issue in the bound project, addressed by its project-local IID.
type Issue struct {
	ID          int64  `json:"id"`
	IID         int64  `json:"iid"`
	ProjectID   int64  `json:"project_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	WebURL      string `json:"web_url"`
}

// MergeRequest is a same-project merge request. Fork MRs are refused.
type MergeRequest struct {
	ID              int64  `json:"id"`
	IID             int64  `json:"iid"`
	ProjectID       int64  `json:"project_id"`
	SourceProjectID int64  `json:"source_project_id"`
	TargetProjectID int64  `json:"target_project_id"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	State           string `json:"state"`
	WebURL          string `json:"web_url"`
	SHA             string `json:"sha"`
	SourceBranch    string `json:"source_branch"`
	TargetBranch    string `json:"target_branch"`
	Draft           bool   `json:"draft"`
}

// DetailedStatus preserves pipeline-level diagnostic evidence, not a verdict.
type DetailedStatus struct {
	Text string `json:"text"`
}

// Pipeline is pipeline identity, status and optional GitLab diagnostics.
type Pipeline struct {
	ID             int64          `json:"id"`
	ProjectID      int64          `json:"project_id"`
	SHA            string         `json:"sha"`
	Ref            string         `json:"ref"`
	Status         string         `json:"status"`
	WebURL         string         `json:"web_url"`
	YAMLErrors     string         `json:"yaml_errors"`
	FailureReason  string         `json:"failure_reason"`
	DetailedStatus DetailedStatus `json:"detailed_status"`
}

// Bridge is a trigger job from trigger_jobs (or legacy bridges). Only its own
// job and parent pipeline identity are retained; downstream_pipeline is not an
// instruction to traverse another pipeline or widen the project binding.
// GitLab exposes the same evidence fields for trigger and build jobs.
type Bridge Job

// Job is a current build or trigger job and its reported parent pipeline.
type Job struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Stage        string   `json:"stage"`
	Status       string   `json:"status"`
	AllowFailure bool     `json:"allow_failure"`
	WebURL       string   `json:"web_url"`
	Pipeline     Pipeline `json:"pipeline"`
}
