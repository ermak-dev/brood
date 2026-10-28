package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/substrate"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (s *Server) pushTemplate(w http.ResponseWriter, r *http.Request, sw app.Swarm, repo string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResourceBody))
	if err != nil {
		writeDetail(w, 413, "template exceeds size limit")
		return
	}
	var request struct {
		Image    string          `json:"image"`
		Template json.RawMessage `json:"actor_template"`
		Policy   json.RawMessage `json:"egress_policy"`
	}
	if json.Unmarshal(body, &request) != nil {
		writeDetail(w, 400, "malformed template request")
		return
	}
	prefix := s.push.imageHost + "/" + repo + "@"
	digest := strings.TrimPrefix(request.Image, prefix)
	if !strings.HasPrefix(request.Image, prefix) || !pushSHA.MatchString(digest) {
		writeDetail(w, 400, "image must be a digest in this swarm repository")
		return
	}
	var template ateapi.ActorTemplate
	var policy ateapi.EgressPolicy
	if substrate.Unmarshal(request.Template, &template) != nil || substrate.Unmarshal(request.Policy, &policy) != nil || !pushLabel.MatchString(template.GetMetadata().GetName()) {
		writeDetail(w, 400, "invalid template or egress policy")
		return
	}
	if len(template.Containers) == 0 || len(template.Containers) > 10 || len(template.Volumes) > 32 {
		writeDetail(w, 400, "invalid template containers or volumes")
		return
	}
	for _, volume := range template.Volumes {
		if volume.GetImage() != nil {
			writeDetail(w, 400, "image-backed volumes are not allowed for push")
			return
		}
	}
	for _, container := range template.Containers {
		if container == nil || (container.Image != "" && container.Image != request.Image) {
			writeDetail(w, 400, "additional container images must match the pushed image")
			return
		}
		container.Image = request.Image
	}
	// Runtime-политика вложена в actor; golden policy — отдельная preview-возможность.
	if unsafePushPolicy(policy.ProtoReflect()) {
		writeDetail(w, 400, "external credential references are not supported by push")
		return
	}
	if template.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode() == ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED {
		writeDetail(w, 400, "template-only push requires golden snapshots disabled")
		return
	}
	if template.SnapshotConfig == nil {
		template.SnapshotConfig = &ateapi.SnapshotConfig{}
	}
	template.SnapshotConfig.GoldenSnapshotConfig = &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}
	template.GoldenEgressPolicy = nil
	scoped(&template, sw.AtespaceName)
	// Resource identity and status are server-owned and excluded from retries.
	template.Metadata = &ateapi.ResourceMetadata{Name: template.Metadata.Name, Atespace: sw.AtespaceName}
	status := s.push.head(r, repo, "manifests", digest)
	if status != 200 {
		if status == 404 {
			writeDetail(w, 400, "image manifest does not exist")
		} else {
			writeDetail(w, 502, "registry request failed")
		}
		return
	}
	if s.ateapi == nil {
		writeDetail(w, 503, "substrate is not configured")
		return
	}
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	operation := &nativeWorkloadOperation{server: s, context: r.Context(), swarm: sw, action: "create-template", template: &template}
	err = s.app.AdmitWorkload(user.ID, sw.ID, operation)
	if err != nil {
		var api *substrate.APIError
		if !errors.As(err, &api) || api.Status != 409 {
			apiError(w, r, err)
			return
		}
		existing, getErr := s.ateapi.GetActorTemplate(r.Context(), sw.AtespaceName, template.Metadata.Name)
		if getErr != nil {
			apiError(w, r, getErr)
			return
		}
		existing = proto.Clone(existing).(*ateapi.ActorTemplate)
		existing.Metadata = &ateapi.ResourceMetadata{Name: existing.GetMetadata().GetName(), Atespace: sw.AtespaceName}
		existing.Status = nil
		if !proto.Equal(existing, &template) {
			writeDetail(w, 409, "template already exists with different content")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"image": request.Image, "template": template.Metadata.Name, "project": sw.ID,
		"egress_policy_deferred": len(policy.Rules) > 0})
}

func unsafePushPolicy(message protoreflect.Message) bool {
	unsafe := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Name() == "credential_uri" && value.String() != "" {
			unsafe = true
			return false
		}
		if field.Message() != nil {
			if field.IsList() {
				list := value.List()
				for i := 0; i < list.Len(); i++ {
					unsafe = unsafe || unsafePushPolicy(list.Get(i).Message())
				}
			} else {
				unsafe = unsafe || unsafePushPolicy(value.Message())
			}
		}
		return !unsafe
	})
	return unsafe
}
