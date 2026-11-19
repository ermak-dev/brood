//go:build linux

package guest

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var errInboxScaffold = errors.New("ordinary Claude plugin state is not an empty private scaffold")

// The ordinary pinned app creates this skeleton even when it installed no
// plugins. In quiescence accept only private, empty directories, not any
// actor-controlled data, and never read or rewrite their contents.
func (p stepPolicy) verifyEmptyInboxPluginScaffold(home string) error {
	fd, err := openSecretDirectory(filepath.Join(home, "plugins"), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errInboxScaffold
	}
	plugins := os.NewFile(uintptr(fd), "ordinary plugin scaffold")
	defer plugins.Close()
	if !p.privateInboxScaffoldDirectory(fd) {
		return errInboxScaffold
	}
	names, err := plugins.Readdirnames(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return errInboxScaffold
	}
	if len(names) == 0 {
		return nil
	}
	if len(names) != 1 || names[0] != "marketplaces" {
		return errInboxScaffold
	}
	child, err := secretChildDirectory(fd, "marketplaces", false)
	if err != nil {
		return errInboxScaffold
	}
	leaf := os.NewFile(uintptr(child), "ordinary marketplace scaffold")
	defer leaf.Close()
	if !p.privateInboxScaffoldDirectory(child) {
		return errInboxScaffold
	}
	entries, err := leaf.Readdirnames(1)
	if len(entries) != 0 || !errors.Is(err, io.EOF) {
		return errInboxScaffold
	}
	return nil
}

func (p stepPolicy) privateInboxScaffoldDirectory(fd int) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Uid == uint32(p.ActorUID) && st.Gid == uint32(p.ActorGID) && st.Mode == unix.S_IFDIR|0700
}
