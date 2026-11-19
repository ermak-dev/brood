//go:build linux

package guest

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const ptyStartRelease = 1

func (p *actorProcess) ptyStartIdentity() (inboxProcessIdentity, error) {
	if p.ptyStart == nil || p.cmd == nil || p.cmd.Process == nil {
		return inboxProcessIdentity{}, errors.New("held actor process is unavailable")
	}
	return inboxProcessInfo(p.cmd.Process.Pid)
}

func (p *actorProcess) releasePTYStart() error {
	writer := p.ptyStart
	if writer == nil {
		return errors.New("PTY startup gate is unavailable")
	}
	p.ptyStart = nil
	defer writer.Close()
	if n, err := writer.Write([]byte{ptyStartRelease}); err != nil || n != 1 {
		return errors.New("PTY startup gate could not be released")
	}
	return nil
}

func awaitPTYStart(fd uintptr) error {
	file := os.NewFile(fd, "actor-pty-start")
	if file == nil {
		return errors.New("inherited PTY startup gate is required")
	}
	defer file.Close()
	unix.CloseOnExec(int(fd))
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("inherited PTY startup gate is not a pipe")
	}
	var release [1]byte
	if _, err := io.ReadFull(file, release[:]); err != nil || release[0] != ptyStartRelease {
		return errors.New("supervisor did not release PTY startup")
	}
	return nil
}
