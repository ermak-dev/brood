package clientapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"brood.dev/go/brood/oci"
)

// Swarm is an accessible project workspace, backed by one server-owned atespace.
type Swarm struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
	State string `json:"state"`
}

// PushResult identifies a registered template and immutable worker image.
type PushResult struct {
	Image                string `json:"image"`
	Template             string `json:"template"`
	Project              string `json:"project"`
	EgressPolicyDeferred bool   `json:"egress_policy_deferred"`
}

// SwarmsList lists only workspaces visible to the current user.
func (c *Client) SwarmsList(ctx context.Context, project string) ([]Swarm, error) {
	var result []Swarm
	err := c.call(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(project)+"/swarms", nil, &result)
	return result, err
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader, size int64, media string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, body)
	if err != nil {
		return nil, errors.New("invalid API request")
	}
	req.ContentLength = size
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		req.Header.Set("Origin", c.origin)
	}
	stored := c.Stored()
	if stored.Session != "" {
		req.AddCookie(&http.Cookie{Name: cookieSession, Value: stored.Session})
	}
	if stored.CSRF != "" {
		req.AddCookie(&http.Cookie{Name: cookieCSRF, Value: stored.CSRF})
		if method != http.MethodGet && method != http.MethodHead {
			req.Header.Set(headerCSRF, stored.CSRF)
		}
	}
	transport := *c.http
	transport.Timeout = timeout
	response, err := transport.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		var cert *tls.CertificateVerificationError
		if errors.As(err, &cert) {
			return nil, errors.New("TLS certificate verification failed; configure a trusted CA")
		}
		return nil, fmt.Errorf("request to %s failed; check network and TLS configuration", c.origin)
	}
	return response, nil
}

func (c *Client) stream(ctx context.Context, method, path string, body io.Reader, size int64, media string, out any) (int, error) {
	resp, err := c.request(ctx, method, path, body, size, media, 90*time.Second)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, ErrRedirect
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, ErrSessionExpired
	}
	if resp.StatusCode == http.StatusNotFound && method == http.MethodHead {
		return resp.StatusCode, nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return resp.StatusCode, fmt.Errorf("%s %s: server error (status %d): %s", method, path, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	c.absorbCookies(resp)
	if out != nil && resp.StatusCode != http.StatusNoContent {
		data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 || json.Unmarshal(data, out) != nil {
			return resp.StatusCode, errors.New("malformed push response")
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	return resp.StatusCode, nil
}

var pushPart = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var pushProjectID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// PushArchive uploads verified blobs in bounded chunks, then manifests by digest.
func (c *Client) PushArchive(ctx context.Context, project, environment string, archive *oci.Archive) (string, error) {
	if !pushProjectID.MatchString(project) || !pushPart.MatchString(environment) {
		return "", errors.New("invalid push project or environment")
	}
	base := "/api/swarms/" + project + "/builds/" + environment
	for _, blob := range archive.Blobs {
		status, err := c.stream(ctx, http.MethodHead, base+"/blobs/"+blob.Digest, nil, 0, "", nil)
		if err != nil {
			return "", err
		}
		if status == http.StatusOK {
			continue
		}
		var upload struct {
			ID string `json:"upload_id"`
		}
		if _, err := c.stream(ctx, http.MethodPost, base+"/uploads", nil, 0, "", &upload); err != nil {
			return "", err
		}
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(upload.ID) {
			return "", errors.New("invalid upload identifier from server")
		}
		path := base + "/uploads/" + upload.ID
		reader, err := archive.Reader(blob.Digest)
		if err != nil {
			return "", err
		}
		const chunk = 32 << 20
		for offset := int64(0); offset < blob.Size; {
			size := min(int64(chunk), blob.Size-offset)
			if _, err := c.stream(ctx, http.MethodPatch, path, io.NewSectionReader(reader, offset, size), size, "application/octet-stream", nil); err != nil {
				return "", err
			}
			offset += size
		}
		if _, err := c.stream(ctx, http.MethodPut, path+"?digest="+url.QueryEscape(blob.Digest), nil, 0, "", nil); err != nil {
			return "", err
		}
	}
	image := ""
	for _, manifest := range archive.Manifests {
		var result struct {
			Image string `json:"image"`
		}
		if _, err := c.stream(ctx, http.MethodPut, base+"/manifests/"+manifest.Digest, bytes.NewReader(manifest.Data), int64(len(manifest.Data)), manifest.MediaType, &result); err != nil {
			return "", err
		}
		if manifest.Digest == archive.Digest {
			image = result.Image
		}
	}
	if image == "" {
		return "", errors.New("server did not confirm uploaded image")
	}
	return image, nil
}

// PushTemplate registers a verified image and its scoped template without starting actors.
func (c *Client) PushTemplate(ctx context.Context, project, environment, image string, template, policy json.RawMessage) (*PushResult, error) {
	if !pushProjectID.MatchString(project) || !pushPart.MatchString(environment) {
		return nil, errors.New("invalid push project or environment")
	}
	data, err := json.Marshal(struct {
		Image    string          `json:"image"`
		Template json.RawMessage `json:"actor_template"`
		Policy   json.RawMessage `json:"egress_policy"`
	}{image, template, policy})
	if err != nil {
		return nil, errors.New("invalid template bundle")
	}
	var result PushResult
	_, err = c.stream(ctx, http.MethodPost, "/api/swarms/"+project+"/builds/"+environment+"/templates", bytes.NewReader(data), int64(len(data)), "application/json", &result)
	return &result, err
}
