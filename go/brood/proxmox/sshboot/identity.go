// Package sshboot reaches a freshly created container over SSH as root to
// install the agent and to run provisioning commands. The Proxmox API cannot
// execute anything inside a container, so the key injected at creation is the
// only way in. The package is silent: it returns data and errors only.
package sshboot

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"
)

// keyComment is the comment of the public key in authorized_keys.
const keyComment = "brood"

// maxKeyFile bounds how much of a key file is read; an OpenSSH ed25519 key
// is about 400 bytes.
const maxKeyFile = 64 << 10

// Identity is the installation's SSH key pair used to reach containers.
type Identity struct {
	signer ssh.Signer
}

// LoadOrCreateIdentity reads an ed25519 private key in OpenSSH PEM format from
// path, or creates it with mode 0600. The parent directory must exist. The
// file is created exclusively, so two processes cannot both create it; when
// it appears concurrently, it is read. A file with group or other permission
// bits, a symlink or a non-regular file is refused.
func LoadOrCreateIdentity(path string) (*Identity, error) {
	id, err := loadIdentity(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	if err := createIdentity(path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return loadIdentity(path)
}

// AuthorizedKey returns the public key as one authorized_keys line with the
// comment "brood".
func (i *Identity) AuthorizedKey() string {
	return authorizedKey(i.signer.PublicKey()) + " " + keyComment
}

// authorizedKey renders a public key in authorized_keys form without a
// comment and without the trailing newline.
func authorizedKey(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// loadIdentity reads and checks the key file. A missing file is reported
// with an error matching fs.ErrNotExist.
func loadIdentity(path string) (*Identity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("sshboot: identity %s: %w", path, err)
	}
	if err := checkKeyFile(path, info); err != nil {
		return nil, err
	}
	// O_NOFOLLOW and the second check on the open descriptor close the gap
	// between Lstat and open; O_NONBLOCK keeps a swapped-in FIFO from hanging.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("sshboot: identity %s: %w", path, err)
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, fmt.Errorf("sshboot: identity %s: %w", path, err)
	}
	if err := checkKeyFile(path, info); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, fmt.Errorf("sshboot: identity %s: %w", path, err)
	}
	if len(data) > maxKeyFile {
		return nil, fmt.Errorf("sshboot: identity %s: file is too large for a private key", path)
	}
	// Parse errors are not wrapped: only their kind is reported, never the
	// bytes of the file.
	raw, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("sshboot: identity %s: not an unencrypted OpenSSH private key", path)
	}
	key, ok := raw.(*ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("sshboot: identity %s: key is not ed25519", path)
	}
	signer, err := ssh.NewSignerFromKey(*key)
	if err != nil {
		return nil, fmt.Errorf("sshboot: identity %s: unusable ed25519 key", path)
	}
	return &Identity{signer: signer}, nil
}

// checkKeyFile refuses anything but a regular file private to its owner.
func checkKeyFile(path string, info fs.FileInfo) error {
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		return fmt.Errorf("sshboot: identity %s: refusing a symlink", path)
	case !mode.IsRegular():
		return fmt.Errorf("sshboot: identity %s: not a regular file", path)
	case mode.Perm()&0o077 != 0:
		return fmt.Errorf("sshboot: identity %s: mode %04o is open to group or others, want 0600", path, mode.Perm())
	}
	return nil
}

// createIdentity generates a key and puts it at path. The key is written to a
// temporary file first and hard-linked into place: link fails when path
// exists, and a concurrent reader never sees a half-written key. An existing
// path is reported with an error matching fs.ErrExist.
func createIdentity(path string) error {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("sshboot: generate identity: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(private, keyComment)
	if err != nil {
		return fmt.Errorf("sshboot: encode identity: %w", err)
	}
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	// CreateTemp opens with O_EXCL and mode 0600.
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("sshboot: create identity %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(pem.EncodeToMemory(block))
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Link(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("sshboot: create identity %s: %w", path, err)
	}
	return nil
}
