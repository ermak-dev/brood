//go:build linux

package guest

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

type inboxProcessImage uint8

const (
	inboxUnknownProcess inboxProcessImage = iota
	inboxGuestProcess
	inboxClaudeProcess
)

type inboxProcessIdentity struct {
	pid, parent int
	start       string
	uid         uint32
	image       inboxProcessImage
}

// matchesInboxConsumer recognizes a fixed helper directly spawned by the
// pinned Claude executable of this root-started actor. This is a local handoff
// fence, not native admission evidence or isolation from a compromised actor.
func matchesInboxConsumer(actor, claude, helper, expectedActor inboxProcessIdentity) bool {
	return actor == expectedActor && actor.image == inboxGuestProcess && actor.uid == 1000 && actor.pid > 1 && actor.start != "" &&
		claude.parent == actor.pid && claude.image == inboxClaudeProcess && claude.uid == 1000 && claude.start != "" &&
		helper.parent == claude.pid && helper.image == inboxGuestProcess && helper.uid == 1000 && helper.start != "" &&
		actor.pid != claude.pid && claude.pid != helper.pid && actor.pid != helper.pid
}

func (r *Runtime) authenticateInboxConsumer(ctx context.Context, connection *net.UnixConn, launch *stepLaunch) (string, error) {
	if launch == nil || ctx.Err() != nil || os.Geteuid() != 0 || launch.actorInbox.pid == 0 {
		return "", ErrInboxHandoffUnavailable
	}
	cred, err := inboxHelperCredentials(connection)
	if err != nil || cred == nil || cred.Uid != 1000 || cred.Gid != 1000 {
		return "", ErrInboxHandoffUnavailable
	}
	helper, err := inboxProcessInfo(int(cred.Pid))
	if err != nil {
		return "", err
	}
	claude, err := inboxProcessInfo(helper.parent)
	if err != nil {
		return "", err
	}
	actor, err := inboxProcessInfo(claude.parent)
	if err != nil || !matchesInboxConsumer(actor, claude, helper, launch.actorInbox) {
		return "", ErrInboxHandoffUnavailable
	}
	// Lifecycle revocation and process inspection are separate. Recheck the
	// exact root launch after the kernel observations before authorizing Take.
	r.stepMu.Lock()
	defer r.stepMu.Unlock()
	if r.stepLaunch != launch || launch.ctx.Err() != nil || ctx.Err() != nil || r.stopping() {
		return "", ErrInboxHandoffUnavailable
	}
	if !launch.ready {
		return "", ErrInboxHandoffWaiting
	}
	return launch.id, nil
}

func inboxProcessStat(pid int) (inboxProcessIdentity, error) {
	if pid <= 1 {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	file, err := os.Open("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(raw) > 4096 {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	last := strings.LastIndexByte(string(raw), ')')
	if last < 0 {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	fields := strings.Fields(string(raw[last+1:]))
	if len(fields) < 20 || fields[0] == "Z" || strings.EqualFold(fields[0], "X") {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 1 {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	return inboxProcessIdentity{pid: pid, parent: ppid, start: fields[19]}, nil
}

func inboxProcessInfo(pid int) (inboxProcessIdentity, error) {
	dir := "/proc/" + strconv.Itoa(pid)
	before, err := inboxProcessStat(pid)
	if err != nil {
		return inboxProcessIdentity{}, err
	}
	directory, err := os.Stat(dir)
	if err != nil {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	st, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 1000 {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	image, err := os.Stat(dir + "/exe")
	if err != nil {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	exe, ok := image.Sys().(*syscall.Stat_t)
	if !ok || exe.Uid != 0 || !image.Mode().IsRegular() || image.Mode().Perm()&0022 != 0 {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	after, err := inboxProcessStat(pid)
	if err != nil || before != after {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	afterImage, err := os.Stat(dir + "/exe")
	if err != nil || !os.SameFile(image, afterImage) {
		return inboxProcessIdentity{}, ErrInboxHandoffUnavailable
	}
	for kind, path := range map[inboxProcessImage]string{inboxGuestProcess: "/proc/self/exe", inboxClaudeProcess: "/opt/brood/bin/claude"} {
		if pinned, err := os.Stat(path); err == nil && os.SameFile(image, pinned) {
			after.image = kind
			break
		}
	}
	if after.image == inboxUnknownProcess {
		return inboxProcessIdentity{}, fmt.Errorf("%w: consumer executable is not pinned", ErrInboxHandoffUnavailable)
	}
	after.uid = st.Uid
	return after, nil
}
