package build

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
)

// ValidateOCIArchive checks an uncompressed OCI layout tar without extracting it.
// It accepts only regular files/directories and requires a versioned layout,
// a nonempty image index, and the blobs referenced by that index.
func ValidateOCIArchive(filename string) error {
	file, err := os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("could not read OCI archive")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("OCI archive must be a regular file")
	}
	reader := tar.NewReader(file)
	documents := map[string][]byte{}
	sizes := map[string]int64{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid OCI archive tar")
		}
		name := strings.TrimSuffix(header.Name, "/")
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe OCI archive entry")
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("unsafe OCI archive entry type")
		}
		if _, ok := sizes[name]; ok {
			return fmt.Errorf("duplicate OCI archive entry")
		}
		sizes[name] = header.Size
		if name == "oci-layout" || name == "index.json" {
			if header.Size > maxDocumentBytes {
				return fmt.Errorf("OCI archive metadata exceeds 1 MiB")
			}
			data, err := io.ReadAll(reader)
			if err != nil {
				return fmt.Errorf("invalid OCI archive metadata")
			}
			documents[name] = data
		} else {
			if _, err := io.Copy(io.Discard, reader); err != nil {
				return fmt.Errorf("invalid OCI archive data")
			}
		}
	}
	var layout struct {
		Version string `json:"imageLayoutVersion"`
	}
	var index struct {
		Version   int `json:"schemaVersion"`
		Manifests []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"manifests"`
	}
	if json.Unmarshal(documents["oci-layout"], &layout) != nil || layout.Version != "1.0.0" || json.Unmarshal(documents["index.json"], &index) != nil || index.Version != 2 || len(index.Manifests) == 0 {
		return fmt.Errorf("OCI archive requires a valid layout and image index")
	}
	for _, manifest := range index.Manifests {
		if !patternImageDigest.MatchString(manifest.Digest) || manifest.Size < 0 {
			return fmt.Errorf("invalid OCI archive descriptor")
		}
		size, exists := sizes["blobs/sha256/"+strings.TrimPrefix(manifest.Digest, "sha256:")]
		if !exists || size != manifest.Size {
			return fmt.Errorf("OCI archive index references a missing or invalid blob")
		}
	}
	return nil
}
