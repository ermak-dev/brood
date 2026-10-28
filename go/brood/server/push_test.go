package server

// FEATURE: Session-scoped OCI push into an authorized swarm repository.
// SPEC: Auth and CSRF, opaque uploads, bounded streaming, scoped digests and templates.
// IMPLEMENTATION: Authorized registry bridge and template-only Substrate publication.
// RESULT: IMPLEMENTED — scoped publication and idempotent retry verified.
// STATUS: DONE

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func pushDigest(body string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))) }

func TestPushConfig(t *testing.T) {
	for _, env := range []map[string]string{
		{"BROOD_REGISTRY_URL": "http://registry:5000"},
		{"BROOD_REGISTRY_URL": "http://u:p@registry", "BROOD_REGISTRY_IMAGE_HOST": "localhost:5002"},
		{"BROOD_REGISTRY_URL": "http://registry/v2", "BROOD_REGISTRY_IMAGE_HOST": "localhost:5002"},
		{"BROOD_REGISTRY_URL": "http://registry", "BROOD_REGISTRY_IMAGE_HOST": "evil/path"},
	} {
		if _, err := LoadConfig(func(k string) string { return env[k] }); err == nil {
			t.Errorf("accepted invalid registry settings %v", env)
		}
	}
}

func TestPushFlow(t *testing.T) {
	var repo string
	var location = ""
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v2/"+repo+"/") {
			t.Errorf("foreign registry path %s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		switch {
		case r.Method == "HEAD":
			w.Header().Set("Content-Length", "6")
			w.WriteHeader(200)
		case r.Method == "POST":
			target := location
			if target == "" {
				target = "/v2/" + repo + "/blobs/uploads/private-registry-id?_state=secret"
			}
			w.Header().Set("Location", target)
			w.WriteHeader(202)
		case r.Method == "PATCH":
			data, _ := io.ReadAll(r.Body)
			if string(data) != "layer" {
				t.Errorf("chunk %q", data)
			}
			w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/private-registry-id?_state=next")
			w.WriteHeader(202)
		case r.Method == "PUT":
			w.WriteHeader(201)
		default:
			w.WriteHeader(500)
		}
	}))
	defer registry.Close()
	ts := newSubstrateServer(t, newFakeAteapi(), func(c *Config) {
		c.RegistryURL = registry.URL
		c.RegistryImageHost = "localhost:5002"
	})
	_, alice, bob, id := actorsSetup(t, ts.testServer)
	repo = swarmAtespace(t, ts.srv, id) + "/dev"
	base := "/api/swarms/" + id + "/builds/dev"
	resp, data, _ := ts.do(alice, "POST", base+"/uploads", "", nil)
	expectStatus(t, "CSRF", resp, data, 403)
	resp, data, _ = ts.do(bob, "POST", base+"/uploads", ts.csrf(bob), nil)
	expectStatus(t, "foreign", resp, data, 404)
	resp, data, _ = ts.do(alice, "POST", base+"/uploads", ts.csrf(alice), nil)
	expectStatus(t, "start", resp, data, 201)
	upload, ok := data["upload_id"].(string)
	if !ok || upload == "" || strings.Contains(upload, "private") {
		t.Fatalf("opaque upload: %v", data)
	}
	other := newClient(t)
	ts.login(other, "alice", "alice-password-1")
	resp, data, _ = ts.do(other, "PUT", base+"/uploads/"+upload+"?digest="+pushDigest("layer"), ts.csrf(other), nil)
	expectStatus(t, "cross session", resp, data, 404)
	req, _ := http.NewRequest("PATCH", ts.http.URL+base+"/uploads/"+upload, strings.NewReader("layer"))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Origin", ts.http.URL)
	req.Header.Set("X-CSRFToken", ts.csrf(alice))
	resp, err := alice.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("patch status %d", resp.StatusCode)
	}
	resp, data, _ = ts.do(alice, "PUT", base+"/uploads/"+upload+"?digest="+pushDigest("layer"), ts.csrf(alice), nil)
	expectStatus(t, "finalize", resp, data, 204)
	resp, data, _ = ts.do(alice, "PUT", base+"/uploads/"+upload+"?digest="+pushDigest("layer"), ts.csrf(alice), nil)
	expectStatus(t, "consumed", resp, data, 404)
	location = "https://foreign.invalid/v2/" + repo + "/blobs/uploads/stolen"
	resp, data, _ = ts.do(alice, "POST", base+"/uploads", ts.csrf(alice), nil)
	expectStatus(t, "cross origin location", resp, data, 502)
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + pushDigest("config") + `","size":6},"layers":[]}`
	put := func(digest, body string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest("PUT", ts.http.URL+base+"/manifests/"+digest, strings.NewReader(body))
		req.Header.Set("Origin", ts.http.URL)
		req.Header.Set("X-CSRFToken", ts.csrf(alice))
		req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		resp, err := alice.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var data map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&data)
		return resp, data
	}
	resp, data = put(pushDigest("tamper"), manifest)
	expectStatus(t, "tamper", resp, data, 400)
	resp, data = put(pushDigest(manifest), manifest)
	expectStatus(t, "manifest", resp, data, 200)
	image := "localhost:5002/" + repo + "@" + pushDigest(manifest)
	if data["image"] != image {
		t.Fatalf("image %v", data)
	}
	template := map[string]any{"image": image, "actor_template": map[string]any{"metadata": map[string]string{"name": "pushed", "atespace": "foreign"}, "containers": []any{map[string]string{"name": "main", "image": image}}, "resources": map[string]any{"limits": map[string]string{"cpuMillicores": "1000", "memoryBytes": "1048576"}}}, "egress_policy": map[string]any{"rules": []any{map[string]string{"action": "ALLOW", "domain": "example.com"}}}}
	template["egress_policy"] = map[string]any{"metadata": map[string]string{"atespace": "foreign", "name": "foreign"}, "rules": []any{map[string]any{"https": map[string]any{"hostnames": []string{"example.com"}}}}}
	resp, data, _ = ts.do(alice, "POST", base+"/templates", ts.csrf(alice), template)
	expectStatus(t, "template", resp, data, 200)
	resp, data, _ = ts.do(alice, "POST", base+"/templates", ts.csrf(alice), template)
	expectStatus(t, "template retry", resp, data, 200)
	saved := ts.fake.templates[swarmAtespace(t, ts.srv, id)+"/pushed"]
	if saved == nil || saved.Containers[0].Image != image {
		t.Fatalf("scoped template %v", saved)
	}
	if saved.Resources.GetLimits().GetCpuMillicores() != 1000 || saved.Resources.GetLimits().GetMemoryBytes() != 1048576 {
		t.Fatalf("template settings lost: %v", saved)
	}
	// BUG: runtime policy нельзя подменять preview golden policy при snapshots disabled.
	if saved.GoldenEgressPolicy != nil || saved.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode().String() != "GOLDEN_SNAPSHOT_MODE_DISABLED" || data["egress_policy_deferred"] != true {
		t.Fatalf("push can create a golden actor or claim runtime policy was installed: %v, %v", saved, data)
	}
	if len(ts.fake.actors) != 0 {
		t.Fatal("push created actors")
	}
	actorTemplate := template["actor_template"].(map[string]any)
	actorTemplate["snapshotConfig"] = map[string]any{"goldenSnapshotConfig": map[string]string{"mode": "GOLDEN_SNAPSHOT_MODE_ENABLED"}}
	resp, data, _ = ts.do(alice, "POST", base+"/templates", ts.csrf(alice), template)
	expectStatus(t, "golden activation refused", resp, data, 400)
	actorTemplate["snapshotConfig"] = map[string]any{"goldenSnapshotConfig": map[string]string{"mode": "GOLDEN_SNAPSHOT_MODE_DISABLED"}}
	resp, data, _ = ts.do(alice, "POST", base+"/templates", ts.csrf(alice), template)
	expectStatus(t, "explicit disabled retry", resp, data, 200)
	actorTemplate["volumes"] = []any{map[string]any{"name": "foreign", "image": map[string]string{"reference": image}}}
	resp, data, _ = ts.do(alice, "POST", base+"/templates", ts.csrf(alice), template)
	expectStatus(t, "image-backed volume", resp, data, 400)
	delete(actorTemplate, "volumes")
	actorTemplate["containers"] = []any{map[string]string{"name": "main", "image": image}, map[string]string{"name": "extra", "image": "localhost:5002/foreign@" + pushDigest("foreign")}}
	resp, data, _ = ts.do(alice, "POST", base+"/templates", ts.csrf(alice), template)
	expectStatus(t, "additional image", resp, data, 400)
	actorTemplate["containers"] = []any{map[string]string{"name": "main", "image": image}}
	actorTemplate["resources"] = map[string]any{"limits": map[string]string{"cpuMillicores": "2000", "memoryBytes": "1048576"}}
	resp, data, _ = ts.do(alice, "POST", base+"/templates", ts.csrf(alice), template)
	expectStatus(t, "conflicting retry", resp, data, 409)
	template["image"] = "localhost:5002/swarm-foreign/dev@" + pushDigest(manifest)
	resp, data, _ = ts.do(alice, "POST", base+"/templates", ts.csrf(alice), template)
	expectStatus(t, "foreign image", resp, data, 400)
}

func TestPushDisabledRequiresAuthentication(t *testing.T) {
	ts := newTestServer(t)
	_, alice, bob, id := actorsSetup(t, ts)
	path := "/api/swarms/" + id + "/builds/dev/blobs/" + pushDigest("layer")
	for _, tc := range []struct {
		client *http.Client
		status int
	}{{newClient(t), 401}, {bob, 404}, {alice, 503}} {
		resp, data, _ := ts.do(tc.client, "HEAD", path, "", nil)
		expectStatus(t, "push authorization", resp, data, tc.status)
	}
}
