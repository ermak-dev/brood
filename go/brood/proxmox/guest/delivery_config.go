//go:build linux

package guest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

var errDeliveryConfiguration = errors.New("guest delivery image configuration unavailable or unsafe")

// ReadSupervisorDeliveryDirectory reads an image's explicit delivery-state-dir
// file beneath a trusted private root-owned directory. Missing configuration
// preserves legacy delivery; invalid configuration never silently disables it.
func ReadSupervisorDeliveryDirectory(ctx context.Context, directory string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return "", errDeliveryConfiguration
	}
	dir, err := openTrustedDeliveryRoot(directory)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errDeliveryConfiguration
	}
	defer dir.Close()
	return readDeliveryDirectory(ctx, dir, 0)
}

func readDeliveryDirectory(ctx context.Context, dir *os.File, owner uint32) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !deliveryDirectoryOwned(dir, owner, true) {
		return "", errDeliveryConfiguration
	}
	fd, err := unix.Openat(int(dir.Fd()), "delivery-state-dir", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return "", nil
	}
	if err != nil {
		return "", errDeliveryConfiguration
	}
	file := os.NewFile(uintptr(fd), "delivery image configuration")
	defer file.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != owner || st.Mode&07777 != 0600 || st.Nlink != 1 {
		return "", errDeliveryConfiguration
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) == 0 || len(data) > 4096 || data[len(data)-1] != '\n' {
		return "", errDeliveryConfiguration
	}
	path := string(data[:len(data)-1])
	if !utf8.ValidString(path) || strings.ContainsAny(path, "\r\n\x00") || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return "", errDeliveryConfiguration
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return path, nil
}
