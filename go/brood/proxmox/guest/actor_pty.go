//go:build linux

package guest

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptyattach"
	"brood.dev/go/brood/proxmox/guest/ptysession"
	"golang.org/x/sys/unix"
)

func actorPTYConfig(executable string) ptysession.Config {
	return ptysession.Config{Command: []string{executable, "--brood-guest-app"}, Env: actorEnv(), Dir: "/workspace/home", Columns: 80, Rows: 24}
}

func createProductionPTYSocket() (*actorSocket, error) {
	if err := prepareRunDirectory(); err != nil {
		return nil, err
	}
	return createActorSocket(ptysession.SocketPath, 0, 1000)
}

func inheritedActorListener(file *os.File, path string, owner uint32) (net.Listener, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0660 {
		return nil, errors.New("PTY listener pathname is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != owner {
		return nil, errors.New("PTY listener is not supervisor-owned")
	}
	listening, err := unix.GetsockoptInt(int(file.Fd()), unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if err != nil || listening != 1 {
		return nil, errors.New("inherited PTY descriptor is not listening")
	}
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, errors.New("cannot adopt inherited PTY listener")
	}
	if listener.Addr().Network() != "unix" || listener.Addr().String() != path {
		listener.Close()
		return nil, errors.New("inherited PTY endpoint differs")
	}
	return listener, nil
}

func runActorPTY(executable string, driftty, oneShot bool) error {
	inherited := os.NewFile(3, "actor-pty-listener")
	if inherited == nil {
		return errors.New("inherited PTY listener is required")
	}
	unix.CloseOnExec(3)
	listener, err := inheritedActorListener(inherited, ptysession.SocketPath, 0)
	inherited.Close()
	if err != nil {
		return err
	}
	defer listener.Close()
	config := actorPTYConfig(executable)
	if oneShot {
		config.OneShot = true
		config.Command = []string{executable, "--brood-guest-inbox-app"}
	}
	host, err := ptysession.New(config)
	if err != nil {
		return errors.New("cannot initialize actor PTY host")
	}
	defer host.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- host.Serve(ctx, listener) }()
	// Без ttyd (его нет в пакетах Debian, кладёт установщик) PTY-хост живёт
	// один: сессия, сообщения и MCP работают, нет только терминала в браузере.
	// О пропаже сообщает PrepareHost; здесь nil-канал просто не срабатывает.
	var terminal *exec.Cmd
	var terminalDone chan error
	if _, err := os.Stat(ttydPath); !os.IsNotExist(err) {
		terminal = exec.Command(ttydPath, actorArgs(executable, driftty)...)
		terminal.Env = actorEnv()
		terminal.Dir = "/workspace/home"
		terminal.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
		if err := terminal.Start(); err != nil {
			cancel()
			listener.Close()
			return errors.New("cannot start actor terminal transport")
		}
		terminalDone = make(chan error, 1)
		go func() { terminalDone <- terminal.Wait() }()
	}
	var result error
	terminalExited, hostExited := terminal == nil, false
	select {
	case result = <-terminalDone:
		terminalExited = true
	case result = <-serveDone:
		hostExited = true
	case <-ctx.Done():
		result = ctx.Err()
	}
	cancel()
	if !terminalExited {
		_ = terminal.Process.Kill()
		select {
		case <-terminalDone:
		case <-time.After(3 * time.Second):
			result = errors.Join(result, errors.New("terminal transport stop is unconfirmed"))
		}
	}
	result = errors.Join(result, host.Close())
	listener.Close()
	if !hostExited {
		select {
		case err := <-serveDone:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
				result = errors.Join(result, err)
			}
		case <-time.After(3 * time.Second):
			result = errors.Join(result, errors.New("PTY host stop is unconfirmed"))
		}
	}
	return result
}

// RunPTYAttach is the fixed unprivileged raw-stream endpoint launched by ttyd.
// Closing it releases only its attachment, not the held Claude process.
func RunPTYAttach(ctx context.Context) error {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return errors.New("PTY attachment requires UID and GID 1000")
	}
	groups, err := os.Getgroups()
	if err != nil {
		return errors.New("PTY attachment group check failed")
	}
	for _, group := range groups {
		if group != 1000 {
			return errors.New("PTY attachment has unexpected supplementary groups")
		}
	}
	if err := lockPrivileges(); err != nil {
		return err
	}
	return ptyattach.Run(ctx, ptysession.SocketPath, os.Stdin, os.Stdout)
}
