package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
)

const (
	pushManifestType = "application/vnd.oci.image.manifest.v1+json"
	pushIndexType    = "application/vnd.oci.image.index.v1+json"
	pushConfigType   = "application/vnd.oci.image.config.v1+json"
)

type pushDescriptor struct {
	MediaType string          `json:"mediaType"`
	Digest    string          `json:"digest"`
	Size      *int64          `json:"size"`
	URLs      []string        `json:"urls"`
	Data      json.RawMessage `json:"data"`
}
type pushManifest struct {
	SchemaVersion int              `json:"schemaVersion"`
	MediaType     string           `json:"mediaType"`
	Config        *pushDescriptor  `json:"config"`
	Layers        []pushDescriptor `json:"layers"`
	Manifests     []pushDescriptor `json:"manifests"`
	Subject       json.RawMessage  `json:"subject"`
	ArtifactType  string           `json:"artifactType"`
}

func validPushDescriptor(d pushDescriptor, kind string) bool {
	if !pushSHA.MatchString(d.Digest) || d.Size == nil || *d.Size < 0 || *d.Size > pushBlobLimit || len(d.URLs) > 0 || len(d.Data) > 0 {
		return false
	}
	switch kind {
	case "config":
		return d.MediaType == pushConfigType
	case "manifest":
		return d.MediaType == pushManifestType || d.MediaType == pushIndexType
	case "layer":
		switch d.MediaType {
		case "application/vnd.oci.image.layer.v1.tar", "application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.oci.image.layer.v1.tar+zstd":
			return true
		}
	}
	return false
}
func (p *pushRegistry) descriptorStatus(r *http.Request, repo, kind string, d pushDescriptor) int {
	resp, err := p.request(r, "HEAD", p.target(repo, kind+"/"+d.Digest), nil, 0, "")
	if err != nil {
		return 502
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return resp.StatusCode
	}
	if resp.ContentLength != *d.Size {
		return 400
	}
	if kind == "manifests" {
		media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil || media != d.MediaType {
			return 400
		}
	}
	return 200
}

func (s *Server) pushManifest(w http.ResponseWriter, r *http.Request, repo string) {
	digest := r.PathValue("digest")
	if !pushSHA.MatchString(digest) {
		writeDetail(w, 400, "invalid sha256 digest")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResourceBody))
	if err != nil {
		writeDetail(w, 413, "manifest exceeds size limit")
		return
	}
	var manifest pushManifest
	if pushContentDigest(body) != digest || json.Unmarshal(body, &manifest) != nil || manifest.SchemaVersion != 2 || len(manifest.Subject) > 0 || manifest.ArtifactType != "" {
		writeDetail(w, 400, "invalid OCI manifest or digest")
		return
	}
	valid := true
	kind := "blobs"
	refs := []pushDescriptor{}
	switch manifest.MediaType {
	case pushManifestType:
		valid = manifest.Config != nil && len(manifest.Manifests) == 0 && len(manifest.Layers) <= 1024
		if manifest.Config != nil {
			valid = valid && validPushDescriptor(*manifest.Config, "config")
			refs = append(refs, *manifest.Config)
		}
		for _, d := range manifest.Layers {
			valid = valid && validPushDescriptor(d, "layer")
			refs = append(refs, d)
		}
	case pushIndexType:
		valid = manifest.Config == nil && len(manifest.Layers) == 0 && len(manifest.Manifests) > 0 && len(manifest.Manifests) <= 1024
		kind = "manifests"
		for _, d := range manifest.Manifests {
			valid = valid && validPushDescriptor(d, "manifest")
			refs = append(refs, d)
		}
	default:
		valid = false
	}
	if !valid {
		writeDetail(w, 400, "invalid OCI descriptors")
		return
	}
	for _, d := range refs {
		status := s.push.descriptorStatus(r, repo, kind, d)
		if status != 200 {
			if status == 404 || status == 400 {
				writeDetail(w, 400, "referenced content is missing or inconsistent")
			} else {
				writeDetail(w, 502, "registry request failed")
			}
			return
		}
	}
	resp, err := s.push.request(r, "PUT", s.push.target(repo, "manifests/"+digest), bytes.NewReader(body), int64(len(body)), manifest.MediaType)
	if err != nil {
		writeDetail(w, 502, "registry request failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		writeDetail(w, 502, "registry manifest rejected")
		return
	}
	writeJSON(w, 200, map[string]string{"image": s.push.imageHost + "/" + repo + "@" + digest})
}
