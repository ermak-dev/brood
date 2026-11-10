package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"brood.dev/go/brood/pipeline"
)

// ProjectSyncResult describes the server's committed declarative project sync.
// GitCredential is the effective reference to a server-only project Git
// credential; GitCredentialBound reports that it names a credential bound to
// exactly the project repository. Neither carries a credential value.
type ProjectSyncResult struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Repository         string `json:"repository"`
	Created            bool   `json:"created"`
	Changed            bool   `json:"changed"`
	OriginMode         string `json:"origin_mode"`
	GitCredential      string `json:"git_credential"`
	GitCredentialBound bool   `json:"git_credential_bound"`
}

// ValidateWorkflowPublication checks deterministic server limits before project sync.
func ValidateWorkflowPublication(workflows map[string]string) ([]byte, error) {
	if len(workflows) > pipeline.MaxWorkflowDefinitions {
		return nil, errors.New("too many workflow definitions")
	}
	total := 0
	for name, text := range workflows {
		total += len(name) + len(text)
		if len(name) > pipeline.MaxWorkflowNameBytes || len(text) > pipeline.MaxWorkflowDefinitionBytes || total > pipeline.MaxWorkflowBatchBytes {
			return nil, errors.New("workflow definition batch exceeds size limit")
		}
	}
	body, err := json.Marshal(map[string]any{"workflows": workflows})
	if err != nil {
		return nil, errors.New("could not encode workflow definitions")
	}
	if len(body) > pipeline.MaxWorkflowBodyBytes {
		return nil, errors.New("workflow publication request exceeds size limit")
	}
	return body, nil
}

// PublishWorkflows publishes named standalone workflow definitions for a project.
func (c *Client) PublishWorkflows(ctx context.Context, projectID string, workflows map[string]string) error {
	body, err := ValidateWorkflowPublication(workflows)
	if err != nil {
		return err
	}
	var response json.RawMessage
	return c.call(ctx, http.MethodPut, "/api/projects/"+url.PathEscape(projectID)+"/workflows", body, &response)
}

// publicProjectSyncError возвращает только фиксированные публичные сообщения.
func publicProjectSyncError(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 4097))
	if err != nil || len(data) > 4096 {
		return ""
	}
	var response struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(data, &response) != nil {
		return ""
	}
	switch response.Detail {
	case "invalid or oversized project declaration":
		return "invalid or oversized project declaration"
	case "workflow limit exceeded: too many workflow definitions":
		return "workflow limit exceeded: too many workflow definitions"
	case "workflow limit exceeded: workflow definition batch exceeds size limit":
		return "workflow limit exceeded: workflow definition batch exceeds size limit"
	default:
		return ""
	}
}

// ProjectSync synchronizes project inventory using the normal session and CSRF transport.
func (c *Client) ProjectSync(ctx context.Context, declaration map[string]any) (ProjectSyncResult, error) {
	var result ProjectSyncResult
	body, err := json.Marshal(declaration)
	if err != nil {
		return result, errors.New("could not encode project declaration")
	}
	if err := c.call(ctx, http.MethodPut, "/api/projects/sync", body, &result); err != nil {
		return ProjectSyncResult{}, err
	}
	return result, nil
}
