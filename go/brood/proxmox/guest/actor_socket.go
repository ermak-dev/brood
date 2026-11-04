//go:build linux

package guest

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

type actorSocket struct {
	listener *net.UnixListener
	path     string
	info     os.FileInfo
	once     sync.Once
	err      error
}

func createActorSocket(path string, uid, gid int) (*actorSocket, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("PTY socket path must be absolute and clean")
	}
	dir, err := openSecretDirectory(filepath.Dir(path), false)
	if err != nil {
		return nil, errors.New("PTY socket parent must be a real directory")
	}
	defer unix.Close(dir)
	var stat unix.Stat_t
	if unix.Fstat(dir, &stat) != nil || stat.Uid != uint32(uid) || stat.Mode&0022 != 0 {
		return nil, errors.New("PTY socket parent ownership is unsafe")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return nil, errors.New("PTY socket path already exists or cannot be inspected")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("cannot create PTY socket")
	}
	listener.SetUnlinkOnClose(false)
	info, err := os.Lstat(path)
	socket := &actorSocket{listener: listener, path: path, info: info}
	if err != nil {
		listener.Close()
		return nil, errors.New("cannot inspect PTY socket")
	}
	if os.Chown(path, uid, gid) != nil || os.Chmod(path, 0660) != nil {
		socket.Close()
		return nil, errors.New("cannot secure PTY socket")
	}
	return socket, nil
}

func (socket *actorSocket) Close() error {
	socket.once.Do(func() {
		socket.err = socket.listener.Close()
		current, err := os.Lstat(socket.path)
		if os.IsNotExist(err) {
			return
		}
		if err != nil || !os.SameFile(socket.info, current) {
			socket.err = errors.Join(socket.err, errors.New("PTY socket pathname ownership changed"))
			return
		}
		socket.err = errors.Join(socket.err, os.Remove(socket.path))
	})
	return socket.err
}
