package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxCreateBranchBytes bounds a deliberately narrow ASCII branch name.
	MaxCreateBranchBytes = 255
	// MaxCreateTitleBytes bounds the UTF-8 title, more strictly than characters.
	MaxCreateTitleBytes = 255
	// MaxCreateDescriptionBytes bounds caller text, excluding the correlation marker.
	// Eight KiB leaves room in a 64 KiB canonical journal payload even when JSON
	// uses six-byte escaping for every description/title character.
	MaxCreateDescriptionBytes = 8 << 10
	// MaxCreateRequestIDBytes bounds the caller's non-secret correlation ID.
	MaxCreateRequestIDBytes = 64
	createMarker            = "<!-- brood-mr-create:"
)

// CreateMergeRequestInput is explicit, bounded input for a same-project MR.
// SourceSHA is an expected full lowercase Git object ID, not a server-side CAS:
// GitLab's create endpoint offers no SHA precondition. A mismatched reply is
// uncertain and must be reconciled, never replayed. RequestID is non-secret
// correlation metadata, notan idempotency key or authority to create or merge.
// The server caller must generate it before reservation and keep it unique in the
// bound project across actors/incarnations, not copy an actor-local request ID.
// No endpoint, project, credentials, approvals or merge options are accepted.
type CreateMergeRequestInput struct {
	SourceBranch string
	TargetBranch string
	SourceSHA    string
	Title        string
	Description  string
	RequestID    string
}

// CreateEffect describes only what this invocation can prove on failure.
// It never authorizes replay; durable intent and reconciliation belong to caller.
type CreateEffect string

const (
	// CreateNotSent proves local refusal before the HTTP client's invocation.
	CreateNotSent CreateEffect = "not_sent"
	// CreateUncertain means creation may have happened. Even a 4xx response is
	// not treated as proof of non-effect: GitLab documents no blanket guarantee.
	CreateUncertain CreateEffect = "uncertain"
)

// CreateError is a sanitized creation failure. It retains no upstream error,
// request, URL, headers, input or response body. Status is zero without a reply.
// Kind and Status explain the failure, while Effect governs reconciliation.
type CreateError struct {
	Kind   ErrorKind
	Status int
	Effect CreateEffect
}

func (e *CreateError) Error() string {
	return (&Error{Kind: e.Kind, Status: e.Status}).Error() + " (create effect: " + string(e.Effect) + ")"
}

// Is preserves context semantics without exposing an upstream error tree.
func (e *CreateError) Is(target error) bool {
	return (&Error{Kind: e.Kind}).Is(target)
}

// CreateMergeRequest makes one non-replayable POST to the bound project.
// allowedTarget must come from trusted caller policy, not the untrusted request;
// TargetBranch must equal it. The caller must also authorize SourceBranch for the
// exact actor/incarnation. This transport neither checks nor grants authority.
// Before invocation the caller must reserve a durable intent and mark it sending
// in its database. Journal outcomes: nil error -> completed, CreateNotSent ->
// refused, any CreateUncertain -> unknown, regardless of Kind or HTTP Status.
// Unknown outcomes require manual reconciliation by the exact final RequestID
// marker and project/branch/SHA, never an automatic retry. The marker is not an
// authenticity proof. Success is creation evidence, notmerge approval or
// fresh-head authority.
func (c *Client) CreateMergeRequest(ctx context.Context, allowedTarget string, in CreateMergeRequestInput) (MergeRequest, error) {
	refuse := func(kind ErrorKind, status int, effect CreateEffect) (MergeRequest, error) {
		return MergeRequest{}, &CreateError{Kind: kind, Status: status, Effect: effect}
	}
	if !c.validCreate(allowedTarget, in) {
		return refuse(Invalid, 0, CreateNotSent)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	contextError := func(effect CreateEffect, status int) (MergeRequest, error) {
		kind := Canceled
		if ctx.Err() == context.DeadlineExceeded {
			kind = Timeout
		}
		return refuse(kind, status, effect)
	}
	if ctx.Err() != nil {
		return contextError(CreateNotSent, 0)
	}
	body, err := json.Marshal(struct {
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		Title        string `json:"title"`
		Description  string `json:"description"`
	}{in.SourceBranch, allowedTarget, in.Title, in.Description + "\n\n" + createMarker + in.RequestID + " -->"})
	if err != nil {
		return refuse(Invalid, 0, CreateNotSent)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/projects/"+decimal(c.projectID)+"/merge_requests", bytes.NewReader(body))
	if err != nil {
		return refuse(Invalid, 0, CreateNotSent)
	}
	// NewRequest populates GetBody for bytes.Reader. Clear it: even a POST can
	// otherwise be retried on a reused connection's zero-byte write failure.
	// Nonempty body, no GetBody and no idempotency headers prevent replay.
	req.GetBody = nil
	req.Header.Set("PRIVATE-TOKEN", string(*c.token))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", in.RequestID)
	if ctx.Err() != nil {
		return contextError(CreateNotSent, 0)
	}
	// From this point conservatively assume a possible effect, even for dial,
	// TLS and 4xx errors. Never infer non-effect from arbitrary transport errors.
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return contextError(CreateUncertain, 0)
		}
		return refuse(Transport, 0, CreateUncertain)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return refuse(HTTP, resp.StatusCode, CreateUncertain)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if ctx.Err() != nil {
		return contextError(CreateUncertain, resp.StatusCode)
	}
	if err != nil {
		return refuse(Response, resp.StatusCode, CreateUncertain)
	}
	if len(raw) > MaxResponseBytes {
		return refuse(Limit, resp.StatusCode, CreateUncertain)
	}
	var mr MergeRequest
	if json.Unmarshal(raw, &mr) != nil {
		return refuse(Response, resp.StatusCode, CreateUncertain)
	}
	if ctx.Err() != nil {
		return contextError(CreateUncertain, resp.StatusCode)
	}
	if mr.ID <= 0 || mr.IID <= 0 || mr.ProjectID != c.projectID || mr.SourceProjectID != c.projectID || mr.TargetProjectID != c.projectID || mr.SourceBranch != in.SourceBranch || mr.TargetBranch != allowedTarget || mr.SHA != in.SourceSHA || mr.State != "opened" {
		return refuse(Binding, resp.StatusCode, CreateUncertain)
	}
	c.scrub(&mr)
	return mr, nil
}

func (c *Client) validCreate(allowedTarget string, in CreateMergeRequestInput) bool {
	if !createBranch(allowedTarget) || !createBranch(in.SourceBranch) || in.TargetBranch != allowedTarget || in.SourceBranch == allowedTarget || !createSHAValid(in.SourceSHA) {
		return false
	}
	if len(in.Title) == 0 || len(in.Title) > MaxCreateTitleBytes || strings.TrimSpace(in.Title) == "" || !utf8.ValidString(in.Title) {
		return false
	}
	for _, r := range in.Title {
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	if len(in.Description) > MaxCreateDescriptionBytes || !utf8.ValidString(in.Description) || strings.Contains(in.Description, createMarker) {
		return false
	}
	for _, r := range in.Description {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	// GitLab descriptions can execute quick actions. This layer must not let
	// prose smuggle merge/approval/retarget/other mutations into a create call.
	for _, line := range strings.FieldsFunc(in.Description, func(r rune) bool { return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029 }) {
		if strings.HasPrefix(strings.TrimSpace(line), "/") {
			return false
		}
	}
	if len(in.RequestID) == 0 || len(in.RequestID) > MaxCreateRequestIDBytes {
		return false
	}
	for _, r := range in.RequestID {
		if !createAlnum(r) && r != '-' && r != '_' {
			return false
		}
	}
	for _, text := range []string{allowedTarget, in.SourceBranch, in.SourceSHA, in.Title, in.Description, in.RequestID} {
		if strings.Contains(text, string(*c.token)) {
			return false
		}
	}
	return true
}

// createBranch deliberately accepts less than git-check-ref-format --branch:
// ASCII alphanumerics, slash, dot, underscore and hyphen, without symbolic HEAD,
// full refs, dot components, .lock suffixes or ambiguous/option-like names.
func createBranch(branch string) bool {
	if len(branch) == 0 || len(branch) > MaxCreateBranchBytes || branch == "HEAD" || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "refs/") || strings.Contains(branch, "..") {
		return false
	}
	looksLikeSHA := len(branch) == 40
	for _, r := range branch {
		if !createAlnum(r) && !strings.ContainsRune("/._-", r) {
			return false
		}
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			looksLikeSHA = false
		}
	}
	if looksLikeSHA {
		return false
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func createAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

func createSHAValid(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, r := range sha {
		if !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
