package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"brood.dev/go/brood/gitcredential"
)

// ProjectGitCredentials returns names, bound repositories and the active flag
// of the project's server-only Git credentials, never usernames or tokens.
func (c *Client) ProjectGitCredentials(ctx context.Context, project string) ([]gitcredential.Info, error) {
	return c.gitCredentialCall(ctx, http.MethodGet, project, nil)
}

// PutProjectGitCredential creates or rotates one server-only Git credential.
// The token travels once in the request body and is never returned.
func (c *Client) PutProjectGitCredential(ctx context.Context, project string, credential gitcredential.Credential) ([]gitcredential.Info, error) {
	if err := gitcredential.Validate(credential); err != nil {
		return nil, err
	}
	// Credential кодируется в JSON как redacted: поля называются явно.
	return c.gitCredentialCall(ctx, http.MethodPut, project, struct {
		Name       string `json:"name"`
		Repository string `json:"repository"`
		Username   string `json:"username"`
		Token      string `json:"token"`
	}{credential.Name, credential.Repository, credential.Username, credential.Token})
}

// DeleteProjectGitCredentials removes named credentials; the server revokes
// gateway access that used them.
func (c *Client) DeleteProjectGitCredentials(ctx context.Context, project string, names []string) ([]gitcredential.Info, error) {
	if len(names) == 0 || len(names) > gitcredential.MaxPerProject {
		return nil, errors.New("Git credential removal requires 1 to 16 names")
	}
	for _, name := range names {
		if err := gitcredential.ValidateName(name); err != nil {
			return nil, err
		}
	}
	return c.gitCredentialCall(ctx, http.MethodDelete, project, struct {
		Names []string `json:"names"`
	}{names})
}

func (c *Client) gitCredentialCall(ctx context.Context, method, project string, input any) ([]gitcredential.Info, error) {
	if project == "" {
		return nil, errors.New("explicit project is required")
	}
	var body []byte
	if input != nil {
		var err error
		if body, err = json.Marshal(input); err != nil {
			return nil, errors.New("could not encode Git credential request")
		}
	}
	var result struct {
		Items []gitcredential.Info `json:"items"`
	}
	if err := c.call(ctx, method, "/api/projects/"+url.PathEscape(project)+"/secrets/git", body, &result); err != nil {
		// Ошибки декодера и транспорта не должны раскрывать даже часть значения.
		switch {
		case errors.Is(err, ErrSessionExpired):
			return nil, ErrSessionExpired
		case errors.Is(err, ErrRedirect):
			return nil, ErrRedirect
		case errors.Is(err, context.Canceled):
			return nil, context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			return nil, errors.New("Git credential request failed; check server access, limits and response")
		}
	}
	if result.Items == nil {
		result.Items = []gitcredential.Info{}
	}
	return result.Items, nil
}
