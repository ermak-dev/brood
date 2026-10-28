package server

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"brood.dev/go/brood/clientapi"
	"brood.dev/go/brood/oci"
)

// FEATURE: настоящий registry принимает chunks и manifests от пользовательской сессии.
// SPEC: отдельный тестовый registry; Substrate — явно fake, акторы не запускаются.
func TestPushRealRegistry(t *testing.T) {
	registry := os.Getenv("BROOD_TEST_REGISTRY_URL")
	if registry == "" {
		t.Skip("BROOD_TEST_REGISTRY_URL must name a disposable registry")
	}
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake, func(c *Config) { c.RegistryURL = registry; c.RegistryImageHost = "localhost:5002" })
	_, _, _, id := actorsSetup(t, ts.testServer)
	client, err := clientapi.New(ts.http.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Login(context.Background(), "alice", "alice-password-1"); err != nil {
		t.Fatal(err)
	}
	path, digest := registryFixtureArchive(t)
	if directory := os.Getenv("BROOD_TEST_BUILD_DIR"); directory != "" {
		data, err := os.ReadFile(filepath.Join(directory, "build.json"))
		if err != nil {
			t.Fatal(err)
		}
		var metadata struct {
			Digest string `json:"imageDigest"`
		}
		if err := json.Unmarshal(data, &metadata); err != nil {
			t.Fatal(err)
		}
		path, digest = filepath.Join(directory, "image.oci.tar"), metadata.Digest
	}
	archive, err := oci.Open(path, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	image, err := client.PushArchive(context.Background(), id, "integration", archive)
	if err != nil {
		t.Fatal(err)
	}
	template := json.RawMessage(fmt.Sprintf(`{"metadata":{"name":"uploaded-template","atespace":"untrusted"},"containers":[{"name":"main","image":%q}],"resources":{"limits":{"cpuMillicores":"1000","memoryBytes":"1048576"}}}`, image))
	for range 2 {
		result, err := client.PushTemplate(context.Background(), id, "integration", image, template, json.RawMessage(`{"rules":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		if result.Project != id || result.Template != "uploaded-template" {
			t.Fatalf("result %+v", result)
		}
	}
	if _, err := client.PushArchive(context.Background(), id, "integration", archive); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}
	saved := fake.templates[swarmAtespace(t, ts.srv, id)+"/uploaded-template"]
	if saved == nil || saved.Containers[0].Image != image || len(fake.actors) != 0 {
		t.Fatal("template missing or actor created")
	}
}

func registryFixtureArchive(t *testing.T) (string, string) {
	t.Helper()
	layer := bytes.Repeat([]byte("x"), 33<<20)
	layerDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(layer))
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(config))
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":%q,"size":%d}]}`, configDigest, len(config), layerDigest, len(layer)))
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"manifests":[{"digest":%q,"size":%d}]}`, digest, len(manifest)))
	path := filepath.Join(t.TempDir(), "image.oci.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for name, data := range map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`), "index.json": index, "blobs/sha256/" + configDigest[7:]: config, "blobs/sha256/" + layerDigest[7:]: layer, "blobs/sha256/" + digest[7:]: manifest} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path, digest
}
