package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Image states reported by the server.
const (
	ImageBuilding = "building"
	ImageReady    = "ready"
	ImageFailed   = "failed"
	ImageRetired  = "retired"
)

// Image is a project worker image built by the server. Log is the bounded
// tail of the build output; Error is a one-line reason of a failed build.
type Image struct {
	ID        string `json:"id"`
	Pool      string `json:"pool"`
	State     string `json:"state"`
	Snapshot  string `json:"snapshot"`
	Playbook  string `json:"playbook"`
	CTID      int64  `json:"ctid"`
	Error     string `json:"error"`
	Log       string `json:"log"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func imagesPath(project string, image ...string) (string, error) {
	if project == "" {
		return "", errors.New("explicit project is required")
	}
	path := "/api/projects/" + url.PathEscape(project) + "/images"
	for _, id := range image {
		if id == "" {
			return "", errors.New("explicit image is required")
		}
		path += "/" + url.PathEscape(id)
	}
	return path, nil
}

// isImagesPath узнаёт пути образов проекта для показа detail отказа.
func isImagesPath(path string) bool {
	rest, found := strings.CutPrefix(path, "/api/projects/")
	if !found {
		return false
	}
	_, rest, found = strings.Cut(rest, "/")
	return found && (rest == "images" || strings.HasPrefix(rest, "images/") || strings.HasPrefix(rest, "images?"))
}

// publicDetail достаёт detail отказа одной строкой без управляющих символов.
func publicDetail(body io.Reader) string {
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
	detail := strings.Map(func(char rune) rune {
		if char < 32 || (char >= 127 && char <= 159) {
			return ' '
		}
		return char
	}, response.Detail)
	return strings.TrimSpace(detail)
}

// ImageBuild starts a build and returns the image in its initial state.
func (c *Client) ImageBuild(ctx context.Context, project, pool string, snapshot io.Reader, size int64) (Image, error) {
	var image Image
	path, err := imagesPath(project)
	if err != nil {
		return image, err
	}
	if pool != "" {
		path += "?" + url.Values{"pool": {pool}}.Encode()
	}
	if err := c.callReader(ctx, http.MethodPost, path, snapshot, size, "application/x-tar", &image, 5*time.Minute); err != nil {
		return Image{}, err
	}
	return image, nil
}

// Image returns one image of the project with its current build log tail.
func (c *Client) Image(ctx context.Context, project, id string) (Image, error) {
	var image Image
	path, err := imagesPath(project, id)
	if err != nil {
		return image, err
	}
	if err := c.call(ctx, http.MethodGet, path, nil, &image); err != nil {
		return Image{}, err
	}
	return image, nil
}

// Images lists the images of the project, newest first.
func (c *Client) Images(ctx context.Context, project string) ([]Image, error) {
	path, err := imagesPath(project)
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []Image `json:"items"`
	}
	if err := c.call(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// ImageDelete removes an image; the server may finish the removal later.
func (c *Client) ImageDelete(ctx context.Context, project, id string) error {
	path, err := imagesPath(project, id)
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodDelete, path, nil, nil)
}
