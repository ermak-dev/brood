//go:build linux

package managedinbox

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const publicationDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// Publication identifies the exact marketplace validated or installed by Publish.
// ManagedSettings is a fragment for a separate, trusted launcher, not a drop-in
// written by Publish or evidence that the loader uses this tree.
type Publication struct {
	Root            string
	ManagedSettings []byte
}

// Publish installs or validates the embedded marketplace at its fixed guest path.
// It requires explicit opt-in and, when enabled, root. Existing entries are never
// repaired or replaced. Disabled calls and errors return a zero Publication.
// Publish does not change managed settings, launch a session, or activate a mod.
func Publish(optIn bool) (Publication, error) {
	if !optIn {
		return Publication{}, nil
	}
	if os.Geteuid() != 0 {
		return Publication{}, errors.New("managed inbox publication requires root")
	}
	root, err := unix.Open("/", publicationDirFlags, 0)
	if err != nil {
		return Publication{}, fmt.Errorf("managed inbox publication: open /: %w", err)
	}
	defer unix.Close(root)
	return publishFrom(root, 0, 0)
}

func publishFrom(root int, uid, gid uint32) (Publication, error) {
	bundle, _ := Package(true)
	owner := publicationOwner{uid: uid, gid: gid}
	if err := owner.publish(root, bundle.Files); err != nil {
		return Publication{}, fmt.Errorf("managed inbox publication at %s: %w", marketplaceRoot, err)
	}
	return Publication{Root: bundle.Root, ManagedSettings: bundle.ManagedSettings}, nil
}

type publicationOwner struct{ uid, gid uint32 }

func (o publicationOwner) publish(root int, files []File) error {
	if err := o.requireDirectory(root, 0o755); err != nil {
		return fmt.Errorf("/: %w", err)
	}
	opt, err := o.openDirectory(root, "opt")
	if err != nil {
		return fmt.Errorf("/opt: %w", err)
	}
	defer unix.Close(opt)
	brood, err := o.openDirectory(opt, "brood")
	if err != nil {
		return fmt.Errorf("/opt/brood: %w", err)
	}
	defer unix.Close(brood)

	tree, err := publicationLayout(files)
	if err != nil {
		return err
	}
	parent, err := o.openDirectory(brood, marketplaceDirectory)
	if errors.Is(err, unix.ENOENT) {
		parent, err = o.makeDirectory(brood, marketplaceDirectory, 0o755)
		if errors.Is(err, unix.EEXIST) {
			parent, err = o.openDirectory(brood, marketplaceDirectory)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", marketplaceDirectory, err)
	}
	defer unix.Close(parent)

	final, err := o.openDirectory(parent, marketplaceName)
	if errors.Is(err, unix.ENOENT) {
		err = o.install(parent, tree)
		if err == unix.EEXIST {
			// Another publisher won the rename; verify what it installed.
			final, err = o.openDirectory(parent, marketplaceName)
		}
	}
	if err != nil {
		return err
	}
	if final >= 0 {
		if err := o.validateTree(final, tree); err != nil {
			return err
		}
	}
	// This also covers a concurrent publisher that has not synced its rename.
	if err := unix.Fsync(parent); err != nil {
		return fmt.Errorf("sync %s: %w", marketplaceDirectory, err)
	}
	if err := unix.Fsync(brood); err != nil {
		return fmt.Errorf("sync /opt/brood: %w", err)
	}
	return nil
}

func (o publicationOwner) openDirectory(parent int, name string) (int, error) {
	fd, err := unix.Openat(parent, name, publicationDirFlags, 0)
	if err != nil {
		return -1, err
	}
	if err := o.requireDirectory(fd, 0o755); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func (o publicationOwner) requireDirectory(fd int, mode uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != o.uid || st.Gid != o.gid || st.Mode != unix.S_IFDIR|mode {
		return fmt.Errorf("expected directory owned by %d:%d with mode %04o", o.uid, o.gid, mode)
	}
	return nil
}

func (o publicationOwner) makeDirectory(parent int, name string, mode uint32) (int, error) {
	if err := unix.Mkdirat(parent, name, mode); err != nil {
		return -1, err
	}
	fd, err := unix.Openat(parent, name, publicationDirFlags, 0)
	if err == nil {
		if err = o.setNewMode(fd, mode); err == nil {
			err = o.requireDirectory(fd, mode)
		}
		if err == nil {
			return fd, nil
		}
		unix.Close(fd)
	}
	unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
	return -1, err
}

// setNewMode is for entries created by this call, never for a preexisting entry.
func (o publicationOwner) setNewMode(fd int, mode uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != o.uid {
		return fmt.Errorf("new entry is not owned by uid %d", o.uid)
	}
	if st.Gid != o.gid {
		if err := unix.Fchown(fd, -1, int(o.gid)); err != nil {
			return err
		}
	}
	return unix.Fchmod(fd, mode)
}

type publicationTree map[string]publicationEntry

type publicationEntry struct {
	directory publicationTree
	file      File
}

func publicationLayout(files []File) (publicationTree, error) {
	root := make(publicationTree)
	for _, file := range files {
		if file.Path == "." || !fs.ValidPath(file.Path) || file.Mode != 0o644 {
			return nil, fmt.Errorf("invalid embedded entry %q", file.Path)
		}
		parts := strings.Split(file.Path, "/")
		dir := root
		for _, name := range parts[:len(parts)-1] {
			entry, exists := dir[name]
			if !exists {
				entry.directory = make(publicationTree)
				dir[name] = entry
			} else if entry.directory == nil {
				return nil, fmt.Errorf("embedded entry %q conflicts with a file", file.Path)
			}
			dir = entry.directory
		}
		name := parts[len(parts)-1]
		if _, exists := dir[name]; exists {
			return nil, fmt.Errorf("duplicate embedded entry %q", file.Path)
		}
		dir[name] = publicationEntry{file: file}
	}
	return root, nil
}

func (o publicationOwner) install(parent int, tree publicationTree) error {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	stage := ".supervisor-inbox-" + hex.EncodeToString(suffix[:])
	fd, err := o.makeDirectory(parent, stage, 0o700)
	if err != nil {
		return fmt.Errorf("create private staging directory: %w", err)
	}
	defer unix.Close(fd)
	defer unix.Unlinkat(parent, stage, unix.AT_REMOVEDIR)
	ready, err := o.makeDirectory(fd, marketplaceName, 0o755)
	if err != nil {
		return fmt.Errorf("create staged marketplace: %w", err)
	}
	defer unix.Close(ready)
	renamed := false
	defer func() {
		if !renamed {
			discardPublicationTree(ready, tree)
			unix.Unlinkat(fd, marketplaceName, unix.AT_REMOVEDIR)
		}
	}()
	if err := o.writeTree(ready, tree); err != nil {
		return fmt.Errorf("write staged marketplace: %w", err)
	}
	check, err := o.openDirectory(fd, marketplaceName)
	if err != nil {
		return fmt.Errorf("open staged marketplace: %w", err)
	}
	if err := o.validateTree(check, tree); err != nil {
		return fmt.Errorf("verify staged marketplace: %w", err)
	}
	// The complete tree is inaccessible to the actor until this rename. Do not
	// fall back to a replacing rename on filesystems without RENAME_NOREPLACE.
	err = unix.Renameat2(fd, marketplaceName, parent, marketplaceName, unix.RENAME_NOREPLACE)
	if err == unix.EEXIST {
		return err
	}
	if err != nil {
		return fmt.Errorf("install marketplace without replacement: %w", err)
	}
	renamed = true
	return nil
}

func (o publicationOwner) writeTree(fd int, tree publicationTree) error {
	for name, entry := range tree {
		if err := o.writeEntry(fd, name, entry); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return unix.Fsync(fd)
}

func (o publicationOwner) writeEntry(parent int, name string, entry publicationEntry) error {
	if entry.directory != nil {
		fd, err := o.makeDirectory(parent, name, 0o755)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		return o.writeTree(fd, entry.directory)
	}
	fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if _, err := file.Write(entry.file.Data); err != nil {
		return err
	}
	if err := o.setNewMode(fd, uint32(entry.file.Mode)); err != nil {
		return err
	}
	return file.Sync()
}

// Only the staging tree created by this call is discarded, relative to its fd.
func discardPublicationTree(fd int, tree publicationTree) {
	for name, entry := range tree {
		if entry.directory == nil {
			unix.Unlinkat(fd, name, 0)
			continue
		}
		child, err := unix.Openat(fd, name, publicationDirFlags, 0)
		if err == nil {
			discardPublicationTree(child, entry.directory)
			unix.Close(child)
			unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
		}
	}
}

// validateTree takes ownership of fd. Enumerate at most one extra name, so an
// invalid tree cannot require an unbounded read before it is refused.
func (o publicationOwner) validateTree(fd int, tree publicationTree) error {
	dir := os.NewFile(uintptr(fd), "marketplace directory")
	defer dir.Close()
	names, err := dir.Readdirnames(len(tree) + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) != len(tree) {
		return errors.New("marketplace directory has missing or unexpected entries")
	}
	for _, name := range names {
		entry, ok := tree[name]
		if !ok {
			return fmt.Errorf("unexpected marketplace entry %q", name)
		}
		if err := o.validateEntry(fd, name, entry); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func (o publicationOwner) validateEntry(parent int, name string, entry publicationEntry) error {
	if entry.directory != nil {
		fd, err := o.openDirectory(parent, name)
		if err != nil {
			return err
		}
		return o.validateTree(fd, entry.directory)
	}
	want := entry.file
	got, err := o.readFile(parent, name, uint32(want.Mode), int64(len(want.Data)))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want.Data) {
		return errors.New("file differs from the embedded bundle")
	}
	return nil
}

func (o publicationOwner) readFile(parent int, name string, mode uint32, limit int64) ([]byte, error) {
	var st unix.Stat_t
	// Refuse special files before even opening them. The parent is pinned and
	// not writable by the actor; also verify the fd used to read the bytes.
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if err := o.requireFile(st, mode, limit); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if err := o.requireFile(st, mode, limit); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, err
}

func (o publicationOwner) requireFile(st unix.Stat_t, mode uint32, limit int64) error {
	if st.Uid != o.uid || st.Gid != o.gid || st.Mode != unix.S_IFREG|mode || st.Nlink != 1 || st.Size > limit {
		return fmt.Errorf("expected a single-link regular file owned by %d:%d, mode %04o, at most %d bytes", o.uid, o.gid, mode, limit)
	}
	return nil
}
