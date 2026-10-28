package oci

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// FEATURE: OCI читается без распаковки и без доверия именам/хешам архива.
// SPEC: digest проверяется, ссылки/дубли/неизвестный корень отвергаются.
func fixture(t *testing.T, extraName string, corrupt bool) (string, string) {
	t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	cfg := fmt.Sprintf("sha256:%x", sha256.Sum256(config))
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":%q,"size":%d},"layers":[]}`, cfg, len(config)))
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"manifests":[{"digest":%q,"size":%d}]}`, digest, len(manifest)))
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	entries := []struct {
		name string
		data []byte
	}{{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)}, {"index.json", index}, {"blobs/sha256/" + cfg[7:], config}, {"blobs/sha256/" + digest[7:], manifest}}
	if corrupt {
		entries[2].data = []byte("tampered")
	}
	if extraName != "" {
		entries = append(entries, struct {
			name string
			data []byte
		}{extraName, []byte("extra")})
	}
	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path, digest
}

func TestArchiveValidatesAndStreamsGraph(t *testing.T) {
	path, digest := fixture(t, "", false)
	archive, err := Open(path, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if archive.Digest != digest || len(archive.Blobs) != 1 || len(archive.Manifests) != 1 {
		t.Fatalf("wrong graph: %+v", archive)
	}
	reader, err := archive.Reader(archive.Blobs[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != archive.Blobs[0].Digest {
		t.Fatalf("reader: %v", err)
	}
}

func TestArchiveRejectsTamperPathsAndDuplicateEntries(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt bool
	}{{"", true}, {"../escape", false}, {"/absolute", false}, {"index.json", false}} {
		path, digest := fixture(t, test.name, test.corrupt)
		archive, err := Open(path, digest)
		if err == nil {
			archive.Close()
			t.Fatalf("accepted %q corrupt=%v", test.name, test.corrupt)
		}
	}
	path, _ := fixture(t, "", false)
	if archive, err := Open(path, "sha256:"+fmt.Sprintf("%064x", 1)); err == nil {
		archive.Close()
		t.Fatal("accepted different image digest")
	}
}
