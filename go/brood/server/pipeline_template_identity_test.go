package server

// FEATURE: Verify a published native pipeline template identity without mutation.
// SPEC: Resolve the exact swarm namespace and UID; require one digest-pinned
// swarm repository image and reject unsafe snapshot and golden networking settings.
// IMPLEMENTATION: Read the bound namespace and template, then check identity and content.
// RESULT: Verification is read-only and does not authorize creation.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/proto"
)

func TestVerifyPipelineTemplateIdentity(t *testing.T) {
	fake := newFakeAteapi()
	status := http.StatusOK
	var requests []string
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.WriteHeader(status)
	}))
	defer registry.Close()
	ts := newSubstrateServer(t, fake, func(c *Config) { c.RegistryURL = registry.URL; c.RegistryImageHost = "localhost:5002" })
	_, _, _, id := actorsSetup(t, ts.testServer)
	sw, found, err := ts.srv.gateway.SwarmByID(id)
	if err != nil || !found {
		t.Fatal(err)
	}
	image := "localhost:5002/" + sw.AtespaceName + "/dev@" + pushDigest("manifest")
	base := &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Name: "published", Atespace: sw.AtespaceName, Uid: "uid-one"}, Containers: []*ateapi.Container{{Name: "main", Image: image}}, SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}}}
	key := sw.AtespaceName + "/published"
	cases := []struct {
		name string
		edit func(*ateapi.ActorTemplate)
		uid  string
		ok   bool
	}{
		{name: "valid", uid: "uid-one", ok: true},
		{name: "missing uid", uid: ""},
		{name: "replaced", uid: "uid-old"},
		{name: "foreign namespace", uid: "uid-one", edit: func(v *ateapi.ActorTemplate) { v.Metadata.Atespace = "foreign" }},
		{name: "foreign repository", uid: "uid-one", edit: func(v *ateapi.ActorTemplate) {
			v.Containers[0].Image = "localhost:5002/foreign/dev@" + pushDigest("manifest")
		}},
		{name: "foreign registry", uid: "uid-one", edit: func(v *ateapi.ActorTemplate) {
			v.Containers[0].Image = "foreign:5002/" + sw.AtespaceName + "/dev@" + pushDigest("manifest")
		}},
		{name: "unpinned", uid: "uid-one", edit: func(v *ateapi.ActorTemplate) {
			v.Containers[0].Image = "localhost:5002/" + sw.AtespaceName + "/dev:latest"
		}},
		{name: "mixed digest", uid: "uid-one", edit: func(v *ateapi.ActorTemplate) {
			v.Containers = append(v.Containers, &ateapi.Container{Name: "extra", Image: "localhost:5002/" + sw.AtespaceName + "/dev@" + pushDigest("other")})
		}},
		{name: "no containers", uid: "uid-one", edit: func(v *ateapi.ActorTemplate) { v.Containers = nil }},
		{name: "golden default", uid: "uid-one", edit: func(v *ateapi.ActorTemplate) { v.SnapshotConfig = nil }},
		{name: "snapshot location", uid: "uid-one", edit: func(v *ateapi.ActorTemplate) { v.SnapshotConfig.StorageLocation = "s3://foreign" }},
		{name: "golden networking", uid: "uid-one", edit: func(v *ateapi.ActorTemplate) { v.GoldenEgressPolicy = &ateapi.EgressPolicyTemplate{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := proto.CloneOf(base)
			if tc.edit != nil {
				tc.edit(item)
			}
			fake.templates[key] = item
			got, err := ts.srv.verifyPipelineTemplateIdentity(context.Background(), sw, "dev", "published", tc.uid, pushDigest("manifest"))
			if (err == nil) != tc.ok {
				t.Fatalf("identity=%+v error=%v", got, err)
			}
			if tc.ok && (got.UID != tc.uid || got.Image != image || got.Name != "published" || got.Atespace != sw.AtespaceName) {
				t.Fatalf("wrong identity: %+v", got)
			}
		})
	}
	wantRequest := "HEAD /v2/" + sw.AtespaceName + "/dev/manifests/" + pushDigest("manifest")
	if len(requests) != 1 || requests[0] != wantRequest {
		t.Fatalf("registry requests = %v, want %s", requests, wantRequest)
	}
	fake.templates[key] = proto.CloneOf(base)
	for _, digest := range []string{"", "not-a-digest", pushDigest("other")} {
		if _, err := ts.srv.verifyPipelineTemplateIdentity(context.Background(), sw, "dev", "published", "uid-one", digest); err == nil {
			t.Fatalf("expected digest %q accepted", digest)
		}
	}
	if len(requests) != 1 {
		t.Fatal("invalid digest contacted registry")
	}
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		status = code
		_, err := ts.srv.verifyPipelineTemplateIdentity(context.Background(), sw, "dev", "published", "uid-one", pushDigest("manifest"))
		if err == nil || (code == http.StatusNotFound && !strings.Contains(err.Error(), "does not exist")) || (code == http.StatusInternalServerError && !strings.Contains(err.Error(), "registry")) {
			t.Fatalf("registry status %d: %v", code, err)
		}
	}
	registry.Close()
	if _, err := ts.srv.verifyPipelineTemplateIdentity(context.Background(), sw, "dev", "published", "uid-one", pushDigest("manifest")); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("registry transport failure: %v", err)
	}
	delete(fake.templates, key)
	if _, err := ts.srv.verifyPipelineTemplateIdentity(context.Background(), sw, "dev", "published", "uid-one", pushDigest("manifest")); err == nil {
		t.Fatal("missing template accepted")
	}
}
