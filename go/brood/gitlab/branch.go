package gitlab

import (
	"context"
	"net/url"
	"strings"
)

// Branch is an exact observed head, not a lock or a create/merge precondition.
type Branch struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

// Branch resolves one narrow branch name in the client's fixed numeric project.
// The encoded name is a single path segment; no response URL is followed.
func (c *Client) Branch(ctx context.Context, name string) (Branch, error) {
	if !ValidCreateBranch(name) || strings.Contains(name, string(*c.token)) {
		return Branch{}, fail(Invalid)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	remaining := MaxOperationBytes
	var raw struct {
		Name   string `json:"name"`
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if _, err := c.get(ctx, &remaining, "/repository/branches/"+url.PathEscape(name), nil, &raw); err != nil {
		return Branch{}, err
	}
	if raw.Name != name || !createSHAValid(raw.Commit.ID) || strings.Contains(raw.Commit.ID, string(*c.token)) {
		return Branch{}, fail(Binding)
	}
	return Branch{Name: raw.Name, SHA: raw.Commit.ID}, nil
}
