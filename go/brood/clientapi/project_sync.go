package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
