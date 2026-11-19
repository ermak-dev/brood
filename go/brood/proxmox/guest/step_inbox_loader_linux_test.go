//go:build linux

package guest

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// FEATURE: Start the one held inbox app against only the proved protected state.
func TestHeldInboxAppPinsOnlyTheProtectedCache(t *testing.T) {
	p := stepTestPolicy(t)
	layout := stepTestLayout(t, p)
	if _, _, err := prepareInboxClaudeLaunch(layout, io.Discard); err == nil {
		t.Fatal("a free session accepted the managed inbox app launch")
	}
	if err := p.enter(attestIdentity); err != nil {
		t.Fatal(err)
	}
	args, env, err := prepareInboxClaudeLaunch(layout, io.Discard)
	if err != nil || !reflect.DeepEqual(args, stepAppArgs()) {
		t.Fatalf("the managed inbox app changed the step launch policy: %v, %v", args, err)
	}
	const protected = "CLAUDE_CODE_PLUGIN_CACHE_DIR=/opt/brood/managed-plugin-state/supervisor-inbox"
	var overrides []string
	for _, value := range env {
		if strings.HasPrefix(value, "CLAUDE_CODE_PLUGIN_") {
			overrides = append(overrides, value)
		}
	}
	if !reflect.DeepEqual(overrides, []string{protected}) {
		t.Fatalf("the held app did not use exactly its protected cache: %v", overrides)
	}
	_, plain, err := prepareClaudeLaunch(layout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range plain {
		if strings.HasPrefix(value, "CLAUDE_CODE_PLUGIN_") {
			t.Fatal("ordinary step acquired the inbox cache override")
		}
	}
}

func TestInboxLoaderRejectsUnprovedSettingsSourcesWithoutEditingThem(t *testing.T) {
	for _, key := range []string{"baseline", "extra managed source", "managed base", "actor plugin cache", "actor oauth state", "project settings", "project ancestor settings"} {
		t.Run(key, func(t *testing.T) {
			p := stepTestPolicy(t)
			layout := stepTestLayout(t, p)
			if err := p.enter(attestIdentity); err != nil {
				t.Fatal(err)
			}
			var path string
			switch key {
			case "extra managed source":
				path = filepath.Join(filepath.Dir(p.dropIn()), "other-profile.json")
			case "managed base":
				path = filepath.Join(p.Managed, "managed-settings.json")
			case "actor plugin cache":
				path = filepath.Join(p.Home, ".claude", "plugins")
			case "actor oauth state":
				path = filepath.Join(p.Home, ".claude", ".credentials.json")
			case "project settings":
				path = filepath.Join(layout.Project, ".claude")
			case "project ancestor settings":
				parent := privateTemp(t)
				layout.Project = filepath.Join(parent, "project")
				if err := os.Mkdir(layout.Project, 0700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(parent, ".claude")
			}
			if path != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("unchanged fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := p.verifyInboxLoaderSources(layout.Project)
			if (err == nil) != (key == "baseline") {
				t.Fatalf("loader provenance behavior %s: %v", key, err)
			}
			if path != "" && readTestFile(t, path) != "unchanged fixture" {
				t.Fatal("verification changed an existing loader source")
			}
		})
	}
}
