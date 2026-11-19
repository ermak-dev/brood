package managedinbox

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// FEATURE: Package the supervisor inbox mod for explicit managed activation.
// SPEC: The default bundles nothing. Opt-in returns a fixed, isolated marketplace
// with the native prompt adapter and a fragment that keeps allowManagedHooksOnly;
// all files are bounded 0644 data.
func TestPackageRequiresOptIn(t *testing.T) {
	bundle, ok := Package(false)
	if ok || bundle.Root != "" || len(bundle.Files) != 0 || len(bundle.ManagedSettings) != 0 {
		t.Fatal("default returned marketplace files or settings")
	}
}

func TestPackageOptIn(t *testing.T) {
	const root = "/opt/brood/managed-marketplaces/supervisor-inbox"
	const plugin = "plugins/supervisor-inbox/"
	bundle, ok := Package(true)
	if !ok || bundle.Root != root {
		t.Fatalf("expected opt-in bundle at %s, got %q", root, bundle.Root)
	}
	assertJSON(t, bundle.ManagedSettings, `{
		"allowManagedHooksOnly": true,
		"strictKnownMarketplaces": [{"source": "directory", "path": "`+root+`"}],
		"extraKnownMarketplaces": {
			"brood-managed-inbox": {"source": {"source": "directory", "path": "`+root+`"}, "autoUpdate": false}
		},
		"enabledPlugins": {"supervisor-inbox@brood-managed-inbox": true}
	}`)
	want := map[string]string{
		".claude-plugin/marketplace.json": `{
			"name": "brood-managed-inbox", "owner": {"name": "Brood"},
			"plugins": [{"name": "supervisor-inbox", "source": "./plugins/supervisor-inbox"}]
		}`,
		plugin + ".claude-plugin/plugin.json": `{
			"name": "supervisor-inbox", "version": "0.1.0",
			"description": "Submit one claimed supervisor inbox instruction"
		}`,
		plugin + "hooks/hooks.json":  `{"modules": ["./register.js"]}`,
		plugin + "hooks/register.js": "",
	}
	for _, file := range bundle.Files {
		manifest, found := want[file.Path]
		if !found {
			t.Fatalf("unexpected or repeated bundled path %q", file.Path)
		}
		delete(want, file.Path)
		if file.Mode != 0o644 || len(file.Data) == 0 || len(file.Data) > 4096 {
			t.Fatalf("%s: expected non-empty, bounded 0644 data", file.Path)
		}
		if manifest != "" {
			assertJSON(t, file.Data, manifest)
		} else if !bytes.Contains(file.Data, []byte(`on("session.start",`)) ||
			!bytes.Contains(file.Data, []byte(`$.prompt.submit(`)) {
			t.Fatalf("%s: expected the native prompt adapter, not an inert module", file.Path)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing bundled paths: %v", want)
	}
}

func TestPackageReturnsFreshPublicationBytes(t *testing.T) {
	first, _ := Package(true)
	original, _ := Package(true)
	first.ManagedSettings[0] ^= 0xff
	for _, file := range first.Files {
		file.Data[0] ^= 0xff
	}
	got, _ := Package(true)
	if !reflect.DeepEqual(got, original) {
		t.Fatal("mutating one package changed a later package")
	}
}

func assertJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var actual, expected any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("got %s, want %s", got, want)
	}
}
