//go:build linux

package guest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"golang.org/x/sys/unix"
)

var errDeliveryDirectory = errors.New("delivery journal requires a trusted private supervisor directory")

// OpenSupervisorDeliveryJournal opens one actor incarnation beneath an explicit
// existing root-owned state directory. It never repairs or replaces old history.
func OpenSupervisorDeliveryJournal(ctx context.Context, directory string, scope deliverylog.Scope) (*deliverylog.Journal, error) {
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return nil, errDeliveryDirectory
	}
	base, err := openDeliveryRoot(directory)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	return openScopedDeliveryJournal(ctx, base, scope)
}

// OpenSupervisorInbox opens a separate receiving store using the supervisor's
// trusted directory checks. This does not enable or advertise a transport.
func OpenSupervisorInbox(ctx context.Context, directory string, scope deliverylog.Scope) (*deliverylog.Inbox, error) {
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return nil, errDeliveryDirectory
	}
	base, err := openDeliveryRoot(directory)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	return openScopedSupervisorInbox(ctx, base, scope)
}

func deliveryDirectoryOwned(dir *os.File, owner uint32, private bool) bool {
	if dir == nil {
		return false
	}
	var st unix.Stat_t
	if unix.Fstat(int(dir.Fd()), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != owner || st.Mode&0022 != 0 {
		return false
	}
	return !private || st.Mode&0777 == 0700
}

func openDeliveryRoot(path string) (*os.File, error) {
	dir, err := openTrustedDeliveryRoot(path)
	if err != nil {
		return nil, errDeliveryDirectory
	}
	return dir, nil
}

func openTrustedDeliveryRoot(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errDeliveryDirectory
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errDeliveryDirectory
	}
	dir := os.NewFile(uintptr(fd), "delivery root")
	if !deliveryDirectoryOwned(dir, 0, false) {
		dir.Close()
		return nil, errDeliveryDirectory
	}
	for _, name := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if name == "" {
			continue
		}
		next, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		dir.Close()
		if err != nil {
			return nil, err
		}
		dir = os.NewFile(uintptr(next), "delivery parent")
		if !deliveryDirectoryOwned(dir, 0, false) {
			dir.Close()
			return nil, errDeliveryDirectory
		}
	}
	if !deliveryDirectoryOwned(dir, 0, true) {
		dir.Close()
		return nil, errDeliveryDirectory
	}
	return dir, nil
}

func deliveryScopeDirectory(parent *os.File, name string) (*os.File, error) {
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, errDeliveryDirectory
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errDeliveryDirectory
	}
	dir := os.NewFile(uintptr(fd), "delivery scope")
	if !deliveryDirectoryOwned(dir, uint32(os.Geteuid()), true) {
		dir.Close()
		return nil, errDeliveryDirectory
	}
	if err := parent.Sync(); err != nil {
		dir.Close()
		return nil, errDeliveryDirectory
	}
	return dir, nil
}

func openScopedDeliveryJournal(ctx context.Context, base *os.File, scope deliverylog.Scope) (*deliverylog.Journal, error) {
	incarnation, err := openDeliveryIncarnation(ctx, base, scope)
	if err != nil {
		return nil, err
	}
	defer incarnation.Close()
	return deliverylog.OpenDirectory(ctx, incarnation, scope, deliverylog.MaxEntries)
}

func openScopedSupervisorInbox(ctx context.Context, base *os.File, scope deliverylog.Scope) (*deliverylog.Inbox, error) {
	incarnation, err := openDeliveryIncarnation(ctx, base, scope)
	if err != nil {
		return nil, err
	}
	defer incarnation.Close()
	directory, err := deliveryScopeDirectory(incarnation, "supervisor-inbox")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	return deliverylog.OpenInboxDirectory(ctx, directory, scope, deliverylog.MaxInboxEntries)
}

func openDeliveryIncarnation(ctx context.Context, base *os.File, scope deliverylog.Scope) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scope.Incarnation < 1 || len(scope.ActorUID) == 0 || len(scope.ActorUID) > 128 {
		return nil, deliverylog.ErrScope
	}
	for _, c := range scope.ActorUID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return nil, deliverylog.ErrScope
		}
	}
	if !deliveryDirectoryOwned(base, uint32(os.Geteuid()), true) {
		return nil, errDeliveryDirectory
	}
	actor, err := deliveryScopeDirectory(base, scope.ActorUID)
	if err != nil {
		return nil, err
	}
	defer actor.Close()
	return deliveryScopeDirectory(actor, strconv.FormatInt(scope.Incarnation, 10))
}
