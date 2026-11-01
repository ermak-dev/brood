//go:build linux

package guest

import (
	"errors"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func listenSwarmSocket(path string, owner uint32) (*net.UnixListener, *os.File, error) {
	fd, err := unix.Open(path+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, nil, errors.New("swarm broker lock unavailable")
	}
	lock := os.NewFile(uintptr(fd), path+".lock")
	fail := func() (*net.UnixListener, *os.File, error) {
		lock.Close()
		return nil, nil, errors.New("swarm broker ownership unavailable")
	}
	info, err := lock.Stat()
	if err != nil {
		return fail()
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Uid != owner || info.Mode().Perm()&0077 != 0 {
		return fail()
	}
	if unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		return fail()
	}
	info, err = os.Lstat(path)
	if err == nil {
		st, ok = info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSocket == 0 || st.Uid != owner {
			return fail()
		}
		conn, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return fail()
		}
		// Exclusive supervisor ownership and ECONNREFUSED together establish
		// staleness. Unknown failures must not authorize removing a live socket.
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return fail()
		}
		if os.Remove(path) != nil {
			return fail()
		}
	} else if !os.IsNotExist(err) {
		return fail()
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fail()
	}
	return listener, lock, nil
}
