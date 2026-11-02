//go:build linux

package sessionctl

// FEATURE: Live identity is process and socket proof, not a registry CWD claim.
// SPEC: Linux process reads must work without type panics; stale socket PIDs and
// intermediate symlinks fail closed before transcript data can be returned.
// IMPLEMENTATION: SO_PEERCRED, process-start key, procfs and no-follow traversal.
// RESULT: IMPLEMENTED — proof primitives covered without Claude or inference.
// STATUS: DONE

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestProcessInfoCurrentPID(t *testing.T) {
	got, err := processInfo(os.Getpid())
	cwd, _ := os.Getwd()
	if err != nil || got.start == "" || got.cwd != cwd {
		t.Fatalf("%#v %v", got, err)
	}
}
func TestOpenSafeRejectsIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600)
	os.Symlink(outside, filepath.Join(root, "link"))
	if f, err := openSafe(filepath.Join(root, "link", "secret"), false); err == nil {
		f.Close()
		t.Fatal("intermediate symlink opened")
	}
}
func TestIdentityRejectsRegistryPIDWithoutSocketProof(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "sessions"), 0700)
	socket := filepath.Join(root, "s.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cwd, _ := os.Getwd()
	row, _ := json.Marshal(map[string]any{"pid": os.Getpid() + 1, "sessionId": "abc", "cwd": cwd, "messagingSocketPath": socket})
	os.WriteFile(filepath.Join(root, "sessions", "registry.json"), row, 0600)
	if _, err := proveIdentity(context.Background(), root, cwd, nil, false); err == nil {
		t.Fatal("registry PID accepted without peer proof")
	}
}
