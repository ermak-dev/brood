package main

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"

	catalog "brood.dev/go/brood/command"
)

func parseCatalogCLI(args []string) (catalog.Invocation, error) {
	if len(args) < 4 || args[0] != "artifact" || args[1] != "publish" {
		return catalog.ParseCLI(args)
	}
	argv := append([]string(nil), args[:4]...)
	for i := 4; i < len(args); i++ {
		path, ok := strings.CutPrefix(args[i], "--file=")
		if args[i] == "--file" {
			if i+1 == len(args) {
				return catalog.Invocation{}, errors.New("--file needs a local path")
			}
			i++
			path, ok = args[i], true
		}
		if !ok {
			argv = append(argv, args[i])
			continue
		}
		data, err := readArtifactFile(path)
		if err != nil {
			return catalog.Invocation{}, err
		}
		argv = append(argv, "--content-base64="+base64.StdEncoding.EncodeToString(data))
	}
	return catalog.ParseCLI(argv)
}

func readArtifactFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("choose a readable regular file; no version was published")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open the local file; no version was published")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, catalog.MaxArtifactBytes+1))
	if err != nil {
		return nil, errors.New("cannot read the local file; no version was published")
	}
	if len(data) > catalog.MaxArtifactBytes {
		return nil, errors.New("artifact exceeds 64 KiB; choose a smaller file. No version was published")
	}
	return data, nil
}
