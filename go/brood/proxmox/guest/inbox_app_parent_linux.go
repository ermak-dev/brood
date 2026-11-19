//go:build linux

package guest

import (
	"bytes"
	"encoding/json"
	"strconv"

	"golang.org/x/sys/unix"
)

const inboxAppParentFile = "step-inbox-parent.json"

type inboxAppParent struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
}

func inboxAppBinding(pid int, start string) []byte {
	ticks, err := strconv.ParseUint(start, 10, 64)
	if pid <= 1 || err != nil || ticks == 0 || strconv.FormatUint(ticks, 10) != start {
		return nil
	}
	data, _ := json.Marshal(inboxAppParent{PID: pid, Start: start})
	return append(data, '\n')
}

func (p stepPolicy) writeInboxAppParent(actor inboxProcessIdentity) error {
	data := inboxAppBinding(actor.pid, actor.start)
	if len(data) == 0 || actor.uid != 1000 || actor.image != inboxGuestProcess {
		return ErrInboxHandoffUnavailable
	}
	return p.writeRun(inboxAppParentFile, data)
}

func (p stepPolicy) removeInboxAppParent() error { return p.removeRun(inboxAppParentFile) }

// Check the root-written marker against observations of this app's actual
// parent, never against values from argv or the actor's environment.
func verifyInboxAppParent(run string, root, group, pid int, start string) error {
	want := inboxAppBinding(pid, start)
	if len(want) == 0 {
		return ErrInboxHandoffUnavailable
	}
	dir, err := openSecretDirectory(run, false)
	if err != nil {
		return ErrInboxHandoffUnavailable
	}
	defer unix.Close(dir)
	if secureSecretDirectory(dir, root, false) != nil {
		return ErrInboxHandoffUnavailable
	}
	data, stat, err := readSecretFile(dir, inboxAppParentFile, len(want), root, 0440)
	if err != nil || stat.Gid != uint32(group) || !bytes.Equal(data, want) {
		return ErrInboxHandoffUnavailable
	}
	return nil
}
