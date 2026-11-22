//go:build linux

package sessionctl

// FEATURE: Helper Git runs no code the actor planted (#183).
// SPEC: With the production flags and environment, only HOME relocated into
// the fixture, neither hooks of the clone nor a global configuration in HOME
// run anything, and the filter they define converts nothing, in workspace facts and wipe.
// IMPLEMENTATION: Every hook, filter and fsmonitor only creates a marker file;
// the test drives workspaceEngine.handle as is.
// RESULT: IMPLEMENTED with real Git effects confined to temporary fixtures.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plantHostileGit puts hooks into the clone and, into home, a global
// configuration with hooks, a filter and an fsmonitor, and global attributes
// applying that filter to every path. It returns the marker directory.
func plantHostileGit(t *testing.T, root, home string) string {
	t.Helper()
	markers, bin := t.TempDir(), t.TempDir()
	script := func(path, body string) {
		t.Helper()
		writeFile(t, path, "#!/bin/sh\n"+body)
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for scope, dir := range map[string]string{"repository": filepath.Join(root, ".git", "hooks"), "global": filepath.Join(home, "hooks")} {
		for _, name := range []string{"post-checkout", "reference-transaction", "post-index-change"} {
			script(filepath.Join(dir, name), "touch '"+markers+"/"+scope+"-"+name+"'\n")
		}
	}
	filter := filepath.Join(bin, "filter")
	script(filter, "touch '"+markers+"/filter-'\"$1\"\nexec cat\n")
	script(filepath.Join(bin, "fsmonitor"), "touch '"+markers+"/fsmonitor'\nexit 1\n")
	writeFile(t, filepath.Join(home, ".gitconfig"), "[core]\n\thooksPath = "+filepath.Join(home, "hooks")+"\n\tfsmonitor = "+filepath.Join(bin, "fsmonitor")+
		"\n[filter \"marker\"]\n\tclean = "+filter+" clean\n\tsmudge = "+filter+" smudge\n")
	writeFile(t, filepath.Join(home, ".config", "git", "attributes"), "* filter=marker\n")
	return markers
}

func requireNoMarkers(t *testing.T, markers string) {
	t.Helper()
	entries, err := os.ReadDir(markers)
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	for _, entry := range entries {
		ran = append(ran, entry.Name())
	}
	if len(ran) > 0 {
		t.Fatalf("helper Git ran actor code: %s", strings.Join(ran, ", "))
	}
}

// The tree names the filter too: facts and wipe keep the tree's attributes
// but have no driver for it.
const filterEverything = "* filter=marker\n"

func TestWorkspaceRunsNoActorCode(t *testing.T) {
	home := t.TempDir()
	e := workspaceFixtureWith(t, home, map[string]string{".gitattributes": filterEverything})
	markers := plantHostileGit(t, e.root, home)
	writeFile(t, filepath.Join(e.root, "tracked"), "changed\n")
	if r := e.handle(context.Background(), workspaceRequest("workspace", `{}`)); !r.OK || !strings.Contains(r.Text, `"git":"dirty"`) {
		t.Fatalf("facts: %+v", r)
	}
	if r := e.handle(context.Background(), workspaceRequest("wipe", `{"confirm":true}`)); !r.OK {
		t.Fatalf("wipe: %+v", r)
	}
	if readFile(t, filepath.Join(e.root, "tracked")) != "original\n" {
		t.Fatal("wipe did not restore the commit")
	}
	requireNoMarkers(t, markers)
}
