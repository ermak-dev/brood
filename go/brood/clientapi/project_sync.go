package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// ProjectSyncResult describes the server's committed declarative project sync.
type ProjectSyncResult struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Created    bool   `json:"created"`
	Changed    bool   `json:"changed"`
	OriginMode string `json:"origin_mode"`
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
