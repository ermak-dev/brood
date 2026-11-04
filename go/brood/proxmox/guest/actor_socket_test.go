//go:build linux

package guest

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// FEATURE: Only the root supervisor publishes the actor PTY listener (#162).
// SPEC: Existing paths and symlink parents are never adopted or removed; an
// inherited descriptor remains a listener and cleanup removes only its own inode.
func TestActorPTYSocketOwnershipAndDescriptor(t *testing.T) {
	path := filepath.Join(privateTemp(t), "pty.sock")
	socket, err := createActorSocket(path, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0660 {
		t.Fatalf("unsafe PTY socket mode: %v", info.Mode())
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != uint32(os.Getuid()) || stat.Gid != uint32(os.Getgid()) {
		t.Fatal("socket ownership differs")
	}
	fd, err := socket.listener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	inherited, err := net.FileListener(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	if inherited.Addr().Network() != "unix" || inherited.Addr().String() != path {
		t.Fatal("inherited descriptor changed endpoint")
	}
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("owned socket pathname retained")
	}
}

func TestActorPTYSocketRefusesExistingPathAndSymlink(t *testing.T) {
	root := privateTemp(t)
	path := filepath.Join(root, "pty.sock")
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if socket, err := createActorSocket(path, os.Getuid(), os.Getgid()); err == nil {
		socket.Close()
		t.Fatal("foreign path adopted")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "foreign" {
		t.Fatal("foreign path changed")
	}
	alias := filepath.Join(root, "alias")
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	if socket, err := createActorSocket(filepath.Join(alias, "pty.sock"), os.Getuid(), os.Getgid()); err == nil {
		socket.Close()
		t.Fatal("symlink parent accepted")
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Fatal("symlink target modified")
	}
}

func TestInheritedActorListenerRejectsWrongDescriptorAndPath(t *testing.T) {
	root := privateTemp(t)
	path := filepath.Join(root, "pty.sock")
	socket, err := createActorSocket(path, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	fd, err := socket.listener.File()
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	listener, err := inheritedActorListener(fd, path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	other, err := createActorSocket(filepath.Join(root, "other.sock"), os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if listener, err := inheritedActorListener(fd, other.path, uint32(os.Getuid())); err == nil {
		listener.Close()
		t.Fatal("different inherited endpoint accepted")
	}
	regular, err := os.CreateTemp(root, "descriptor")
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	if listener, err := inheritedActorListener(regular, path, uint32(os.Getuid())); err == nil {
		listener.Close()
		t.Fatal("ordinary file accepted as PTY listener")
	}
	if listener, err := inheritedActorListener(fd, path, uint32(os.Getuid()+1)); err == nil {
		listener.Close()
		t.Fatal("wrong socket owner accepted")
	}
}

func TestActorPTYSocketCleanupDoesNotRemoveReplacement(t *testing.T) {
	path := filepath.Join(privateTemp(t), "pty.sock")
	socket, err := createActorSocket(path, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := socket.Close(); err == nil {
		t.Fatal("changed socket ownership not reported")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" {
		t.Fatal("replacement removed")
	}
}
