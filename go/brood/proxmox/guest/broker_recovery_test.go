//go:build linux

package guest

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestBrokerRecoversStaleSocketUnderExclusiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swarm.sock")
	owner := uint32(os.Geteuid())
	first, lock, err := listenSwarmSocket(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	first.SetUnlinkOnClose(false)
	first.Close()
	lock.Close()
	recovered, lock, err := listenSwarmSocket(path, owner)
	if err != nil {
		t.Fatal("stale socket blocked recovery", err)
	}
	defer recovered.Close()
	defer lock.Close()
	if other, otherLock, err := listenSwarmSocket(path, owner); err == nil {
		other.Close()
		otherLock.Close()
		t.Fatal("live broker was replaced")
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal("live listener was damaged", err)
	}
	c.Close()
}

func TestBrokerDoesNotRemoveNonSocketPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swarm.sock")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if listener, lock, err := listenSwarmSocket(path, uint32(os.Geteuid())); err == nil {
		listener.Close()
		lock.Close()
		t.Fatal("non-socket path adopted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep" {
		t.Fatal("unowned path changed", err)
	}
}
