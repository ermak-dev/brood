package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"brood.dev/go/brood/projectsecret"
)

// ProjectSecrets returns names and sizes, never secret contents.
func (c *Client) ProjectSecrets(ctx context.Context, project string) (projectsecret.Metadata, error) {
	return c.secretCall(ctx, http.MethodGet, project, "", nil)
}

// PutProjectSecretFiles replaces the named files in one validated batch.
func (c *Client) PutProjectSecretFiles(ctx context.Context, project string, files []projectsecret.File) (projectsecret.Metadata, error) {
	if len(files) == 0 {
		return projectsecret.Metadata{}, errors.New("secret file batch must not be empty")
	}
	if err := projectsecret.ValidateBatch(files, nil); err != nil {
		return projectsecret.Metadata{}, err
	}
	return c.secretCall(ctx, http.MethodPut, project, "/files", struct {
		Files []projectsecret.File `json:"files"`
	}{files})
}

// PutProjectSecretVars replaces the named variables without returning values.
func (c *Client) PutProjectSecretVars(ctx context.Context, project string, vars map[string]string) (projectsecret.Metadata, error) {
	if len(vars) == 0 {
		return projectsecret.Metadata{}, errors.New("secret variable batch must not be empty")
	}
	if err := projectsecret.ValidateBatch(nil, vars); err != nil {
		return projectsecret.Metadata{}, err
	}
	return c.secretCall(ctx, http.MethodPut, project, "/vars", struct {
		Vars map[string]string `json:"vars"`
	}{vars})
}

// DeleteProjectSecretFiles deletes logical names without reading source files.
func (c *Client) DeleteProjectSecretFiles(ctx context.Context, project string, names []string) (projectsecret.Metadata, error) {
	for _, name := range names {
		if err := projectsecret.ValidateFileName(name); err != nil {
			return projectsecret.Metadata{}, err
		}
	}
	return c.deleteProjectSecrets(ctx, project, "/files", names)
}

// DeleteProjectSecretVars deletes variables without disclosing their values.
func (c *Client) DeleteProjectSecretVars(ctx context.Context, project string, names []string) (projectsecret.Metadata, error) {
	for _, name := range names {
		if err := projectsecret.ValidateVar(name, ""); err != nil {
			return projectsecret.Metadata{}, err
		}
	}
	return c.deleteProjectSecrets(ctx, project, "/vars", names)
}

func (c *Client) deleteProjectSecrets(ctx context.Context, project, suffix string, names []string) (projectsecret.Metadata, error) {
	if len(names) == 0 {
		return projectsecret.Metadata{}, errors.New("secret names must not be empty")
	}
	return c.secretCall(ctx, http.MethodDelete, project, suffix, struct {
		Names []string `json:"names"`
	}{names})
}

func (c *Client) secretCall(ctx context.Context, method, project, suffix string, input any) (projectsecret.Metadata, error) {
	var result projectsecret.Metadata
	if project == "" {
		return result, errors.New("explicit project is required")
	}
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		if err != nil {
			return result, errors.New("could not encode secret request")
		}
	}
	err := c.call(ctx, method, "/api/projects/"+url.PathEscape(project)+"/secrets"+suffix, body, &result)
	if err != nil {
		// Ошибки декодера и транспорта не должны раскрывать даже часть значения.
		switch {
		case errors.Is(err, ErrSessionExpired):
			return projectsecret.Metadata{}, ErrSessionExpired
		case errors.Is(err, ErrRedirect):
			return projectsecret.Metadata{}, ErrRedirect
		case errors.Is(err, context.Canceled):
			return projectsecret.Metadata{}, context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			return projectsecret.Metadata{}, context.DeadlineExceeded
		default:
			return projectsecret.Metadata{}, errors.New("secret request failed; check server access and response")
		}
	}
	return result, nil
}
