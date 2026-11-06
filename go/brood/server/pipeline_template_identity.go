package server

import (
	"context"
	"fmt"
	"net/http"

	"brood.dev/go/brood/app"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// pipelineTemplateIdentity records what was observed, not a creation authorization.
// Actor creation must bind this exact namespace and template identity using checked
// preconditions; a separate read cannot prevent replacement or modification afterward.
// Runtime actor networking and snapshot selection also require independent enforcement.
type pipelineTemplateIdentity struct {
	Atespace    string
	AtespaceUID string
	Name        string
	UID         string
	Version     int64
	Image       string
}

func (s *Server) verifyPipelineTemplateIdentity(ctx context.Context, sw app.Swarm, environment, name, uid, expectedDigest string) (pipelineTemplateIdentity, error) {
	var identity pipelineTemplateIdentity
	fail := func(reason string) (pipelineTemplateIdentity, error) {
		return identity, fmt.Errorf("%w: %s", app.ErrConflict, reason)
	}
	if s.ateapi == nil || s.push == nil {
		return fail("native substrate or registry is not configured")
	}
	if !pushLabel.MatchString(environment) || !pushLabel.MatchString(name) || uid == "" || !pushSHA.MatchString(expectedDigest) {
		return fail("template identity or environment is invalid")
	}
	if err := checkNativeNamespace(ctx, s.ateapi, sw); err != nil {
		return identity, err
	}
	template, err := s.ateapi.GetActorTemplate(ctx, sw.AtespaceName, name)
	if err != nil {
		return identity, err
	}
	meta := template.GetMetadata()
	if meta.GetName() != name || meta.GetAtespace() != sw.AtespaceName || meta.GetUid() != uid || meta.GetDeleteTime() != nil {
		return fail("template identity changed or is terminating")
	}
	if template.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode() != ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED || template.GetSnapshotConfig().GetStorageLocation() != "" || template.GetGoldenEgressPolicy() != nil {
		return fail("template snapshot or golden networking configuration is unsupported")
	}
	if len(template.GetContainers()) == 0 || len(template.GetContainers()) > 10 || len(template.GetVolumes()) > 32 {
		return fail("invalid template containers or volumes")
	}
	for _, volume := range template.GetVolumes() {
		if volume == nil || volume.GetImage() != nil {
			return fail("image-backed or invalid volume is unsupported")
		}
	}
	repo := sw.AtespaceName + "/" + environment
	image := s.push.imageHost + "/" + repo + "@" + expectedDigest
	for _, container := range template.GetContainers() {
		if container == nil || container.GetImage() != image {
			return fail("container image does not match the expected digest in the swarm repository")
		}
	}
	status := s.push.head((&http.Request{}).WithContext(ctx), repo, "manifests", expectedDigest)
	if status == http.StatusNotFound {
		return fail("image manifest does not exist")
	}
	if status != http.StatusOK {
		return fail("registry request failed")
	}
	return pipelineTemplateIdentity{Atespace: sw.AtespaceName, AtespaceUID: sw.AtespaceUID, Name: name, UID: uid, Version: meta.GetVersion(), Image: image}, nil
}
