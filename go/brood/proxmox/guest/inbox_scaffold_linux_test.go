//go:build linux

package guest

import (
	"os"
	"path/filepath"
	"testing"
)

// BUG: The ordinary pinned app creates plugins/marketplaces even without plugin data.
// HYPOTHESIS: A path-existence check refuses this empty scaffold before the held app.
func TestInboxLoaderAllowsOnlyEmptyOrdinaryPluginScaffold(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"absent", true},
		{"empty plugins", true},
		{"ordinary empty marketplaces", true},
		{"existing marketplace data", false},
		{"actor registry", false},
		{"unexpected child", false},
		{"writable scaffold", false},
		{"symlink scaffold", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := stepTestPolicy(t)
			layout := stepTestLayout(t, p)
			if err := p.enter(attestIdentity); err != nil {
				t.Fatal(err)
			}
			plugins := filepath.Join(p.Home, ".claude", "plugins")
			market := filepath.Join(plugins, "marketplaces")
			var marker string
			mkdir := func(dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.name {
			case "absent":
			case "empty plugins":
				mkdir(plugins)
			case "ordinary empty marketplaces":
				mkdir(market)
			case "existing marketplace data":
				mkdir(filepath.Join(market, "foreign"))
			case "actor registry":
				mkdir(market)
				marker = filepath.Join(plugins, "known_marketplaces.json")
				if err := os.WriteFile(marker, []byte("fixture only"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unexpected child":
				mkdir(filepath.Join(plugins, "cache"))
			case "writable scaffold":
				mkdir(market)
				if err := os.Chmod(market, 0770); err != nil {
					t.Fatal(err)
				}
			case "symlink scaffold":
				mkdir(plugins)
				if err := os.Symlink(privateTemp(t), market); err != nil {
					t.Fatal(err)
				}
			}
			err := p.verifyInboxLoaderSources(layout.Project)
			if (err == nil) != tc.ok {
				t.Fatalf("empty-scaffold verdict for %s: %v", tc.name, err)
			}
			if tc.name != "absent" {
				if _, err := os.Lstat(plugins); err != nil {
					t.Fatal("verification removed the actor's plugin directory")
				}
			}
			if marker != "" && readTestFile(t, marker) != "fixture only" {
				t.Fatal("verification modified actor registry data")
			}
		})
	}
}
