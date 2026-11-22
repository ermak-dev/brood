package server

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"time"

	"brood.dev/go/brood/build"
)

func readImageSnapshot(w http.ResponseWriter, r *http.Request) (*os.File, string, error) {
	file, err := os.CreateTemp("", "brood-image-*")
	if err != nil {
		return nil, "", errors.New("could not stage the build snapshot")
	}
	accepted := false
	defer func() {
		if !accepted {
			file.Close()
		}
	}()
	// Сборка владеет открытым дескриптором; авария сервера не оставит исходники на диске.
	if err := os.Remove(file.Name()); err != nil {
		return nil, "", errors.New("could not stage the build snapshot")
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Minute))
	defer controller.SetReadDeadline(time.Time{})
	if r.ContentLength > build.MaxSnapshotBytes {
		return nil, "", &http.MaxBytesError{Limit: build.MaxSnapshotBytes}
	}
	input := http.MaxBytesReader(w, r.Body, build.MaxSnapshotBytes)
	defer input.Close()
	digest := sha256.New()
	if err := copyImageSnapshot(tar.NewWriter(io.MultiWriter(file, digest)), tar.NewReader(input)); err != nil {
		return nil, "", err
	}
	var trailing [512]byte
	for {
		n, err := input.Read(trailing[:])
		for _, b := range trailing[:n] {
			if b != 0 {
				return nil, "", errors.New("invalid build snapshot: data follows the USTAR archive")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", snapshotReadError(err)
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", errors.New("could not rewind the build snapshot")
	}
	accepted = true
	return file, hex.EncodeToString(digest.Sum(nil)), nil
}

func copyImageSnapshot(dst *tar.Writer, src *tar.Reader) error {
	seen := make(map[string]bool)
	for {
		h, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return snapshotReadError(err)
		}
		if h.Format != tar.FormatUSTAR || h.Typeflag != tar.TypeReg || !build.ValidSnapshotPath(h.Name) || build.ExcludedSnapshotPath(h.Name) || seen[h.Name] || h.Size < 0 || h.Size > build.MaxSnapshotBytes || len(seen) >= build.MaxSnapshotFiles {
			return errors.New("invalid build snapshot: expected unique regular files in a USTAR archive")
		}
		seen[h.Name] = true
		clean := &tar.Header{Name: h.Name, Format: tar.FormatUSTAR, Typeflag: tar.TypeReg, Mode: 0o644 | (h.Mode & 0o111), Size: h.Size}
		if err := dst.WriteHeader(clean); err != nil {
			return errors.New("could not stage the build snapshot")
		}
		if _, err := io.CopyN(dst, src, h.Size); err != nil {
			return snapshotReadError(err)
		}
	}
	if !seen[build.Filename] {
		return errors.New("the build snapshot must contain brood.yaml at its root")
	}
	for name := range seen {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return errors.New("invalid build snapshot: one path is both a file and a directory")
			}
		}
	}
	if err := dst.Close(); err != nil {
		return errors.New("could not stage the build snapshot")
	}
	return nil
}

func snapshotReadError(err error) error {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		return err
	}
	return errors.New("could not read the build snapshot; send a complete USTAR archive")
}
