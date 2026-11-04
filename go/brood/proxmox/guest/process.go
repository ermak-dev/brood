//go:build linux

package guest

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// UID 1000 is exclusive to this actor in the dedicated CT. A child may setsid, so
// killing a process group alone cannot establish that actor networking stopped.
func drainActorProcesses(ctx context.Context, sweep func() (bool, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		alive, err := sweep()
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func actorStatus(status []byte) (bool, error) {
	var state string
	var ids []string
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "State:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				state = fields[1]
			}
		}
		if strings.HasPrefix(line, "Uid:") {
			ids = strings.Fields(strings.TrimPrefix(line, "Uid:"))
		}
	}
	if state == "" || len(ids) != 4 {
		return false, errors.New("cannot inspect actor process identity")
	}
	actor := false
	for _, id := range ids {
		uid, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			return false, errors.New("invalid actor process identity")
		}
		actor = actor || uid == 1000
	}
	return actor && state != "Z" && state != "X", nil
}
func sweepActorUID() (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, errors.New("cannot enumerate actor processes")
	}
	alive := false
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return false, errors.New("actor cleanup requires pidfd access")
		}
		// Pin identity before reading /proc so PID reuse cannot kill a replacement.
		status, err := os.ReadFile("/proc/" + entry.Name() + "/status")
		if os.IsNotExist(err) {
			_ = unix.Close(fd)
			continue
		}
		if err != nil {
			_ = unix.Close(fd)
			return false, errors.New("cannot read actor process identity")
		}
		actor, err := actorStatus(status)
		if err != nil {
			_ = unix.Close(fd)
			return false, err
		}
		if actor {
			alive = true
			err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		}
		_ = unix.Close(fd)
		if err != nil && !errors.Is(err, unix.ESRCH) {
			return false, errors.New("cannot terminate actor process")
		}
	}
	return alive, nil
}
