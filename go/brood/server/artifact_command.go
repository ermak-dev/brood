package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"brood.dev/go/brood/command"
	"brood.dev/go/brood/store"
)

func artifactPath(swarmID, versionID string) string {
	return "/artifacts/" + url.PathEscape(swarmID) + "/" + url.PathEscape(versionID)
}

func (s *Server) artifactURL(swarmID, versionID string) string {
	return s.artifactOrigin + artifactPath(swarmID, versionID)
}

func (s *Server) executeArtifactCommand(ctx context.Context, swarmID string, inv command.Invocation) (command.Result, error) {
	var args struct {
		Name      string `json:"name"`
		MediaType string `json:"media_type"`
		Content   string `json:"content_base64"`
		VersionID string `json:"version_id"`
	}
	_ = json.Unmarshal(inv.Arguments, &args)
	var v store.ArtifactVersion
	var err error
	if inv.Name == "artifact_publish" {
		content, decodeErr := base64.StdEncoding.Strict().DecodeString(args.Content)
		if decodeErr != nil {
			return command.Result{}, command.Refuse(nil, "invalid base64 content; no version was published")
		}
		if len(content) > command.MaxArtifactBytes {
			return command.Result{}, command.Refuse(nil, "artifact exceeds 64 KiB; publish a smaller file. No version was published")
		}
		v, err = s.store.PublishArtifact(ctx, swarmID, args.Name, args.MediaType, content)
	} else {
		v, err = s.store.GetArtifact(ctx, swarmID, args.VersionID)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return command.Result{}, command.Refuse(nil, "artifact version or active swarm not found")
	case errors.Is(err, store.ErrInvalidArtifactName), errors.Is(err, store.ErrInvalidArtifactMediaType), errors.Is(err, store.ErrInvalidArtifactContent), errors.Is(err, store.ErrArtifactTooLarge), errors.Is(err, store.ErrArtifactQuotaExceeded):
		return command.Result{}, command.Refuse(nil, err.Error()+"; no version was published")
	case err != nil:
		if inv.Name == "artifact_publish" {
			return command.Result{Outcome: "unconfirmed"}, command.Refuse(nil, "publication is unconfirmed; do not retry blindly")
		}
		return command.Result{}, command.Refuse(nil, "artifact version unavailable; retry the read")
	}
	link := s.artifactURL(swarmID, v.ID)
	data := map[string]any{"version_id": v.ID, "name": v.Name, "version": v.Version, "media_type": v.MediaType, "url": link}
	if inv.Name == "artifact_get" {
		data["content_base64"] = v.Content
	}
	raw, _ := json.Marshal(data)
	return command.Result{Text: fmt.Sprintf("%s · version %d: %s", v.Name, v.Version, link), Data: raw, Outcome: "completed"}, nil
}
