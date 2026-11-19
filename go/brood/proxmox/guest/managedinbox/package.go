// Package managedinbox bundles a native prompt adapter for a managed local
// marketplace. Nothing activates it by default. On Linux, Publish is an explicit
// opt-in for the marketplace files only; it does not change launch settings or
// open a supervisor inbox.
//
// The opt-in launcher uses the fixed root-owned marketplace and pre-installed
// registries and cache checked by VerifyProtectedPluginState. It merges the
// fragment into the root-owned step drop-in without dropping any restrictions.
// This layout pins a dedicated protected state directory, not precedence over
// actor-writable plugin state. Alternate loader and managed policy sources are
// refused before launching the held app; the default actor has no cache override.
//
// The adapter runs the fixed /opt/brood/bin/brood-guest executable with the
// single argument --brood-guest-inbox and working directory /. The host runs it
// as the unprivileged actor, and the helper enforces no new privileges. The
// bundle installs neither the helper nor its broker. The adapter accepts one
// bounded, strictly validated JSON offer and calls native prompt submission
// once, with no fallback to the legacy message helper or terminal input. The
// helper distinguishes a clean, unclaimed wait from a failed claim. Any
// ambiguous result stops polling rather than risking a replay.
//
// The launcher must authenticate the authorized mod invocation independently of
// its JSON before releasing prompt bytes, then enforce a live, single-use permit
// for the exact actor incarnation, launch, native session, inbox identity,
// delivery ID, and payload digest. A fixed argv, actor UID, environment value,
// or echoed ID alone does not establish that authority. Never give the mod direct
// access to the supervisor's inbox. Do not turn a dropped, timed-out, or
// ambiguous submission into a retry. Native turn.start and turn.complete are
// telemetry only; correlate both to the same authorized native session and turn,
// never by prompt text alone. They are not a supervisor custody receipt or a
// completed Brood result. The pinned managed-loading proof did not establish
// either helper authorization or this session/turn correlation contract.
package managedinbox

import (
	"bytes"
	_ "embed"
	"io/fs"
)

const (
	marketplaceDirectory = "managed-marketplaces"
	marketplaceName      = "supervisor-inbox"
	marketplaceRoot      = "/opt/brood/" + marketplaceDirectory + "/" + marketplaceName
	pluginPath           = "plugins/" + marketplaceName + "/"
)

const managedSettings = `{
  "allowManagedHooksOnly": true,
  "strictKnownMarketplaces": [{"source": "directory", "path": "` + marketplaceRoot + `"}],
  "extraKnownMarketplaces": {
    "brood-managed-inbox": {"source": {"source": "directory", "path": "` + marketplaceRoot + `"}, "autoUpdate": false}
  },
  "enabledPlugins": {"supervisor-inbox@brood-managed-inbox": true}
}
`

//go:embed marketplace/.claude-plugin/marketplace.json
var marketplaceManifest []byte

//go:embed marketplace/plugins/supervisor-inbox/.claude-plugin/plugin.json
var pluginManifest []byte

//go:embed marketplace/plugins/supervisor-inbox/hooks/hooks.json
var hookManifest []byte

//go:embed marketplace/plugins/supervisor-inbox/hooks/register.js
var hookModule []byte

// File holds static data for publication at a path relative to the bundle root.
type File struct {
	Path string
	Mode fs.FileMode
	Data []byte
}

// Bundle contains a fixed marketplace and its managed settings fragment.
type Bundle struct {
	Root            string
	Files           []File
	ManagedSettings []byte
}

// Package returns a fresh bundle only for an explicit opt-in. It performs no
// filesystem operations and does not start or authorize a session or helper.
func Package(optIn bool) (Bundle, bool) {
	if !optIn {
		return Bundle{}, false
	}
	return Bundle{
		Root: marketplaceRoot,
		Files: []File{
			{Path: ".claude-plugin/marketplace.json", Mode: 0o644, Data: bytes.Clone(marketplaceManifest)},
			{Path: pluginPath + ".claude-plugin/plugin.json", Mode: 0o644, Data: bytes.Clone(pluginManifest)},
			{Path: pluginPath + "hooks/hooks.json", Mode: 0o644, Data: bytes.Clone(hookManifest)},
			{Path: pluginPath + "hooks/register.js", Mode: 0o644, Data: bytes.Clone(hookModule)},
		},
		ManagedSettings: []byte(managedSettings),
	}, true
}
