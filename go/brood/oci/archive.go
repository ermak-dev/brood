// Package oci reads content-addressed OCI archives without extracting files.
package oci

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
)

const maxJSON = 4 << 20

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Blob identifies a verified content-addressed archive entry.
type Blob struct {
	Digest string
	Size   int64
}

// Manifest contains a verified manifest, ordered after its dependencies.
type Manifest struct {
	Blob
	MediaType string
	Data      []byte
}

// Archive holds an open file and bounded metadata, never uncompressed layers.
type Archive struct {
	Digest    string
	Blobs     []Blob
	Manifests []Manifest
	file      *os.File
	entries   map[string]entry
}
type entry struct{ offset, size int64 }
type counter struct {
	io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += int64(n)
	return n, err
}

type descriptor struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	Size      int64    `json:"size"`
	URLs      []string `json:"urls"`
}
type document struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
	Manifests     []descriptor `json:"manifests"`
	Subject       *descriptor  `json:"subject"`
}

// Open verifies archive hashes, root identity and the complete referenced graph.
func Open(path, digest string) (*Archive, error) {
	if !digestPattern.MatchString(digest) {
		return nil, errors.New("invalid OCI image digest")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("could not open OCI archive")
	}
	a := &Archive{Digest: digest, file: file, entries: map[string]entry{}}
	success := false
	defer func() {
		if !success {
			file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 32<<30 {
		return nil, errors.New("invalid OCI archive file")
	}
	count := &counter{Reader: file}
	reader := tar.NewReader(count)
	files := map[string]bool{}
	var index []byte
	layout := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid OCI tar archive")
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag == tar.TypeDir {
			if name != "blobs/" && name != "blobs" && name != "blobs/sha256/" && name != "blobs/sha256" && name != "." {
				return nil, errors.New("unexpected OCI archive directory")
			}
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || files[name] || len(files) >= 10000 || header.Size < 0 || header.Size > 8<<30 {
			return nil, errors.New("invalid or duplicate OCI archive entry")
		}
		files[name] = true
		switch {
		case name == "oci-layout":
			if header.Size > 1024 {
				return nil, errors.New("invalid OCI layout")
			}
			var value struct {
				Version string `json:"imageLayoutVersion"`
			}
			data, err := io.ReadAll(reader)
			if err != nil || json.Unmarshal(data, &value) != nil || value.Version != "1.0.0" {
				return nil, errors.New("invalid OCI layout")
			}
			layout = true
		case name == "index.json":
			if header.Size > maxJSON {
				return nil, errors.New("OCI index exceeds size limit")
			}
			index, err = io.ReadAll(reader)
			if err != nil {
				return nil, err
			}
		case strings.HasPrefix(name, "blobs/sha256/"):
			digest := "sha256:" + strings.TrimPrefix(name, "blobs/sha256/")
			if !digestPattern.MatchString(digest) {
				return nil, errors.New("invalid OCI blob path")
			}
			a.entries[digest] = entry{count.n, header.Size}
			hash := sha256.New()
			n, err := io.Copy(hash, reader)
			if err != nil || n != header.Size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != digest {
				return nil, errors.New("OCI blob digest mismatch")
			}
		default:
			return nil, errors.New("unexpected OCI archive entry")
		}
	}
	if !layout || len(index) == 0 {
		return nil, errors.New("OCI layout or index missing")
	}
	var outer document
	if json.Unmarshal(index, &outer) != nil || outer.SchemaVersion != 2 || len(outer.Manifests) == 0 {
		return nil, errors.New("invalid OCI root index")
	}
	rootPresent := fmt.Sprintf("sha256:%x", sha256.Sum256(index)) == digest
	for _, desc := range outer.Manifests {
		if desc.Digest == digest {
			if err := a.checkDescriptor(desc); err != nil {
				return nil, err
			}
			rootPresent = true
		}
	}
	if !rootPresent {
		return nil, errors.New("build digest does not match OCI root index")
	}
	visited := map[string]bool{}
	active := map[string]bool{}
	var visit func(string, []byte, int) error
	visit = func(digest string, data []byte, depth int) error {
		if visited[digest] {
			return nil
		}
		if active[digest] || depth > 16 || len(visited) > 4096 {
			return errors.New("invalid OCI manifest graph")
		}
		active[digest] = true
		if data == nil {
			var err error
			data, err = a.jsonBlob(digest)
			if err != nil {
				return err
			}
		}
		var doc document
		if json.Unmarshal(data, &doc) != nil || doc.SchemaVersion != 2 {
			return errors.New("invalid OCI manifest")
		}
		media := doc.MediaType
		switch media {
		case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
			if len(doc.Manifests) == 0 {
				return errors.New("empty OCI image index")
			}
			for _, child := range doc.Manifests {
				if err := a.checkDescriptor(child); err != nil {
					return err
				}
				if err := visit(child.Digest, nil, depth+1); err != nil {
					return err
				}
			}
		case "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json":
			deps := append([]descriptor{doc.Config}, doc.Layers...)
			for _, child := range deps {
				if err := a.checkDescriptor(child); err != nil {
					return err
				}
				if !visited[child.Digest] {
					a.Blobs = append(a.Blobs, Blob{child.Digest, child.Size})
					visited[child.Digest] = true
				}
			}
			if doc.Subject != nil {
				if err := a.checkDescriptor(*doc.Subject); err != nil {
					return err
				}
				if err := visit(doc.Subject.Digest, nil, depth+1); err != nil {
					return err
				}
			}
		case "":
			if data != nil && digest == a.Digest && len(doc.Manifests) > 0 {
				media = "application/vnd.oci.image.index.v1+json"
				for _, child := range doc.Manifests {
					if err := a.checkDescriptor(child); err != nil {
						return err
					}
					if err := visit(child.Digest, nil, depth+1); err != nil {
						return err
					}
				}
			} else {
				return errors.New("missing OCI manifest media type")
			}
		default:
			return errors.New("unsupported OCI manifest media type")
		}
		a.Manifests = append(a.Manifests, Manifest{Blob: Blob{digest, int64(len(data))}, MediaType: media, Data: data})
		visited[digest] = true
		delete(active, digest)
		return nil
	}
	var root []byte
	if fmt.Sprintf("sha256:%x", sha256.Sum256(index)) == digest {
		root = index
	}
	if err := visit(digest, root, 0); err != nil {
		return nil, err
	}
	success = true
	return a, nil
}

func (a *Archive) checkDescriptor(d descriptor) error {
	e, ok := a.entries[d.Digest]
	if !digestPattern.MatchString(d.Digest) || !ok || d.Size != e.size || d.Size < 0 || len(d.URLs) != 0 {
		return errors.New("invalid or missing OCI descriptor")
	}
	return nil
}
func (a *Archive) jsonBlob(digest string) ([]byte, error) {
	e, ok := a.entries[digest]
	if !ok || e.size > maxJSON {
		return nil, errors.New("OCI manifest missing or exceeds size limit")
	}
	return io.ReadAll(io.NewSectionReader(a.file, e.offset, e.size))
}

// Reader returns a fresh bounded reader for a verified blob.
func (a *Archive) Reader(digest string) (*io.SectionReader, error) {
	e, ok := a.entries[digest]
	if !ok {
		return nil, errors.New("unknown OCI blob")
	}
	return io.NewSectionReader(a.file, e.offset, e.size), nil
}

// Close releases the archive file descriptor.
func (a *Archive) Close() error { return a.file.Close() }
