//go:build linux

package managedinbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	pluginStatePath     = "managed-plugin-state/" + marketplaceName
	pluginStateRoot     = "/opt/brood/" + pluginStatePath
	pluginMarketplace   = "brood-managed-inbox"
	pluginVersion       = "0.1.0"
	pluginCachePath     = pluginMarketplace + "/" + marketplaceName + "/" + pluginVersion
	pluginRegistryLimit = 8192
)

// PluginStateDirectory is the only cache path supported by the managed inbox app.
const PluginStateDirectory = pluginStateRoot

// VerifyProtectedPluginState checks the fixed marketplace, registries, and cache
// as root. It neither provisions them nor establishes how a launcher uses them.
func VerifyProtectedPluginState() error {
	if os.Geteuid() != 0 {
		return errors.New("managed inbox plugin state verification requires root")
	}
	root, err := unix.Open("/", publicationDirFlags, 0)
	if err != nil {
		return fmt.Errorf("managed inbox plugin state: open /: %w", err)
	}
	defer unix.Close(root)
	return verifyPluginStateFrom(root, 0, 0)
}

func verifyPluginStateFrom(root int, uid, gid uint32) error {
	o := publicationOwner{uid: uid, gid: gid}
	if err := o.requireDirectory(root, 0o755); err != nil {
		return fmt.Errorf("/: %w", err)
	}
	brood, err := o.openPluginStateDirectory(root, "opt/brood")
	if err != nil {
		return fmt.Errorf("/opt/brood: %w", err)
	}
	defer unix.Close(brood)

	bundle, _ := Package(true)
	marketTree, err := publicationLayout(bundle.Files)
	if err != nil {
		return err
	}
	market, err := o.openPluginStateDirectory(brood, marketplaceDirectory+"/"+marketplaceName)
	if err == nil {
		err = o.validateTree(market, marketTree)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", marketplaceRoot, err)
	}

	var cacheFiles []File
	for _, file := range bundle.Files {
		if rel, ok := strings.CutPrefix(file.Path, pluginPath); ok {
			file.Path = pluginCachePath + "/" + rel
			cacheFiles = append(cacheFiles, file)
		}
	}
	cacheTree, err := publicationLayout(cacheFiles)
	if err != nil {
		return err
	}
	state, err := o.openPluginStateDirectory(brood, pluginStatePath)
	if err != nil {
		return fmt.Errorf("%s: %w", pluginStateRoot, err)
	}
	dir := os.NewFile(uintptr(state), pluginStateRoot)
	defer dir.Close()
	if err := o.verifyPluginState(dir, cacheTree); err != nil {
		return fmt.Errorf("%s: %w", pluginStateRoot, err)
	}
	return nil
}

// The path is a fixed, relative path; each component is opened and checked on
// its own fd before it can be used as a parent.
func (o publicationOwner) openPluginStateDirectory(root int, path string) (int, error) {
	fd := root
	for _, name := range strings.Split(path, "/") {
		next, err := o.openDirectory(fd, name)
		if fd != root {
			unix.Close(fd)
		}
		if err != nil {
			return -1, fmt.Errorf("%s: %w", name, err)
		}
		fd = next
	}
	return fd, nil
}

func (o publicationOwner) verifyPluginState(dir *os.File, cacheTree publicationTree) error {
	state := int(dir.Fd())
	registries := map[string]pluginRegistryObject{
		"known_marketplaces.json": {
			pluginMarketplace: pluginRegistryObject{
				"source":          pluginRegistryObject{"source": "directory", "path": marketplaceRoot},
				"installLocation": marketplaceRoot,
				"lastUpdated":     pluginRegistryTimestamp{},
			},
		},
		"installed_plugins.json": {
			"version": json.Number("2"),
			"plugins": pluginRegistryObject{
				marketplaceName + "@" + pluginMarketplace: []any{pluginRegistryObject{
					"scope":       "user",
					"version":     pluginVersion,
					"installPath": pluginStateRoot + "/cache/" + pluginCachePath,
					"installedAt": pluginRegistryTimestamp{},
					"lastUpdated": pluginRegistryTimestamp{},
				}},
			},
		},
	}
	// There are only the two registries, the cache, and an optional empty
	// marketplaces directory from the loader. Read one extra name at most.
	names, err := dir.Readdirnames(len(registries) + 3)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, name := range names {
		switch name {
		case "cache":
		case "marketplaces":
			fd, err := o.openDirectory(state, name)
			if err == nil {
				err = o.validateTree(fd, nil)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		default:
			if _, ok := registries[name]; !ok {
				return fmt.Errorf("unexpected plugin state entry %q", name)
			}
		}
	}
	for name, want := range registries {
		data, err := o.readFile(state, name, 0o644, pluginRegistryLimit)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if !matchPluginRegistry(dec, want) {
			return fmt.Errorf("%s: unexpected registry contents", name)
		}
		if _, err := dec.Token(); err != io.EOF {
			return fmt.Errorf("%s: trailing registry data", name)
		}
	}
	cache, err := o.openDirectory(state, "cache")
	if err == nil {
		err = o.validateTree(cache, cacheTree)
	}
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	return nil
}

type pluginRegistryObject map[string]any

type pluginRegistryTimestamp struct{} // Optional RFC 3339 string; not a selection field.

// Match only the fixed schema, consuming each field once. Recursion follows the
// expected shape rather than descending into arbitrary registry input.
func matchPluginRegistry(dec *json.Decoder, want any) bool {
	token, err := dec.Token()
	if err != nil {
		return false
	}
	switch want := want.(type) {
	case pluginRegistryObject:
		if token != json.Delim('{') {
			return false
		}
		remaining := maps.Clone(want)
		for dec.More() {
			key, err := dec.Token()
			name, _ := key.(string)
			value, ok := remaining[name]
			if err != nil || !ok || !matchPluginRegistry(dec, value) {
				return false
			}
			delete(remaining, name)
		}
		for _, value := range remaining {
			if _, optional := value.(pluginRegistryTimestamp); !optional {
				return false
			}
		}
		end, err := dec.Token()
		return err == nil && end == json.Delim('}')
	case []any:
		if token != json.Delim('[') {
			return false
		}
		for _, item := range want {
			if !matchPluginRegistry(dec, item) {
				return false
			}
		}
		end, err := dec.Token()
		return err == nil && end == json.Delim(']')
	case pluginRegistryTimestamp:
		value, _ := token.(string)
		_, err := time.Parse(time.RFC3339, value)
		return err == nil
	default:
		return token == want
	}
}
