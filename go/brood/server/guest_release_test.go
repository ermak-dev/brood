//go:build linux

package server

// FEATURE: The server names the guest release it serves (#133).
// SPEC: The release ID of the server's verified artifact manifest equals the
// ID the guest bootstrap gives the same manifest (guest.ArtifactManifest.
// ReleaseID, #184), the name of releases/<id> in the CT; without a setting or a
// verified manifest the server has no release.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/guest"
)

func TestGuestReleaseIDMatchesBootstrap(t *testing.T) {
	ts := newSubstrateServer(t, newFakeAteapi())
	ts.srv.artifactsDir = writeGuestArtifacts(t)
	manifest, ok := readGuestArtifacts(ts.srv.artifactsDir)
	if !ok {
		t.Fatal("fixture manifest rejected")
	}
	var bootstrap guest.ArtifactManifest
	for _, a := range manifest.Artifacts {
		bootstrap.Artifacts = append(bootstrap.Artifacts, guest.Artifact{Name: a.Name, Size: a.Size, SHA256: a.SHA256})
	}
	want, err := bootstrap.ReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := ts.srv.currentGuestRelease(); !ok || got != want {
		t.Fatalf("server release %q %v, want the bootstrap's %s", got, ok, want)
	}
	// Тот же независимый вектор, что у guest.TestArtifactManifestReleaseID.
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	vector := guestArtifactManifest{Artifacts: []guestArtifactEntry{{"bro", 10, a}, {"brood-guest", 20, b}, {"claude", 30, c}}}
	if got := guestReleaseID(vector); got != "f9e92918e7eee8ea2afc3e83010634025f668241533ce140af9a3a7a662dd12f" {
		t.Fatalf("release id of the shared vector: %s", got)
	}
	for name, change := range map[string]func(){
		"no setting":        func() { ts.srv.artifactsDir = "" },
		"missing manifest":  func() { os.Remove(filepath.Join(ts.srv.artifactsDir, "manifest.json")) },
		"manifest mismatch": func() { os.WriteFile(filepath.Join(ts.srv.artifactsDir, "claude"), []byte("longer claude\n"), 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			ts.srv.artifactsDir = writeGuestArtifacts(t)
			change()
			if got, ok := ts.srv.currentGuestRelease(); ok || got != "" {
				t.Fatalf("release without a verified manifest: %q", got)
			}
		})
	}
}
