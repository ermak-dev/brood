//go:build linux

package guest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"brood.dev/go/brood/actormodel"
)

// ActorSpec contains only fixed actor settings, never supervisor credentials.
type ActorSpec struct {
	Executable string
	Args, Env  []string
	Dir        string
	UID, GID   uint32
}

func actorSpec(executable string) ActorSpec {
	return ActorSpec{Executable: executable, Args: []string{"--brood-guest-actor"}, Env: actorEnv(), Dir: "/workspace/project", UID: 1000, GID: 1000}
}

// actorEnv возвращает фиксированное окружение актора: проектный секрет его
// имён не переопределит. DISABLE_FEEDBACK_COMMAND и
// CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY (#164) выключают /feedback и опрос
// качества сессии: отвечать некому. Окна контекста здесь нет:
// оно свойство модели установки. Прокси-переменных нет: сеть актора не
// ограничена, он ходит обычной сетью и резолвером контейнера.
func actorEnv() []string {
	return []string{"HOME=/workspace/home", "PWD=/workspace/project", "USER=claude", "LOGNAME=claude", "PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "GOTOOLCHAIN=local", "GOMAXPROCS=4", "GOFLAGS=-p=2", "DISABLE_AUTOUPDATER=1", "DISABLE_FEEDBACK_COMMAND=1", "CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1", "TERM=xterm-256color", "LANG=C.UTF-8", "ANTHROPIC_BASE_URL=http://127.0.0.1:8317", "ANTHROPIC_AUTH_TOKEN=brood-local", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1", "BROOD_EXECUTION_MODE=guest"}
}

// claudeModelEnv maps the installation models (#263) onto the model variables
// of pinned Claude Code 2.1.310. The main model is the session model
// (ANTHROPIC_MODEL, which also outranks a /model choice saved in the actor
// home) and the opus and sonnet aliases, so opusplan, agent definitions and
// the Agent tool resolve to it. The small model is the haiku alias, which
// Claude Code also uses for background tasks. Subagents without a model of
// their own inherit the session model, so CLAUDE_CODE_SUBAGENT_MODEL stays
// unset rather than downshifting delegated work. The fable alias stays
// unpinned: Claude Code treats the ANTHROPIC_DEFAULT_FABLE_MODEL name as a
// Fable model in its prompt, labels and fallback.
func claudeModelEnv(m actormodel.Models) []string {
	var env []string
	if m.Model != "" {
		env = append(env, "ANTHROPIC_MODEL="+m.Model, "ANTHROPIC_DEFAULT_OPUS_MODEL="+m.Model, "ANTHROPIC_DEFAULT_SONNET_MODEL="+m.Model)
	}
	if m.Small != "" {
		env = append(env, "ANTHROPIC_DEFAULT_HAIKU_MODEL="+m.Small)
	}
	return env
}

const drifttyIndex = "/usr/local/share/brood/driftty/index.html"

func actorArgs(executable string, driftty bool) []string {
	args := []string{"--interface", "127.0.0.1", "--port", "8080", "--writable", "--check-origin"}
	if driftty {
		args = append(args, "--index", drifttyIndex)
	}
	return append(args, executable, "--brood-guest-attach")
}

// RunActor is the dedicated --brood-guest-actor dispatch target. The parent must
// dispatch it before reading supervisor configuration or opening root services.
// It accepts no actor-controlled arguments and owns the raw PTY host and ttyd.
func RunActor() error {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return errors.New("actor requires UID and GID 1000")
	}
	groups, err := os.Getgroups()
	if err != nil {
		return errors.New("actor group check failed")
	}
	for _, g := range groups {
		if g != 1000 {
			return errors.New("actor has unexpected supplementary groups")
		}
	}
	if err := lockPrivileges(); err != nil {
		return err
	}
	if err := os.Chdir("/workspace/home"); err != nil {
		return errors.New("actor home directory unavailable")
	}
	unix.Umask(0077)
	// Стоковый Debian не несёт driftty: без файла ttyd отдаёт встроенную
	// страницу, с файлом, который положил установщик, — driftty.
	index, err := os.Stat(drifttyIndex)
	if err != nil && !os.IsNotExist(err) {
		return errors.New("cannot inspect driftty terminal index")
	}
	if err == nil && !index.Mode().IsRegular() {
		return errors.New("driftty terminal index is not a regular file")
	}
	self, locateErr := os.Executable()
	if locateErr != nil {
		return errors.New("cannot locate trusted PTY executable")
	}
	return runActorPTY(self, err == nil)
}
func lockPrivileges() error {
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return errors.New("cannot set actor NoNewPrivs")
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return errors.New("cannot clear actor ambient capabilities")
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return errors.New("cannot clear actor capabilities")
	}
	if err := unix.Capget(&header, &data[0]); err != nil {
		return errors.New("cannot verify actor capabilities")
	}
	for _, d := range data {
		if d.Effective != 0 || d.Permitted != 0 || d.Inheritable != 0 {
			return errors.New("actor capabilities remain")
		}
	}
	nnp, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil || nnp != 1 {
		return errors.New("actor NoNewPrivs verification failed")
	}
	return nil
}
func prepareWorkspace(path string, uid, gid int) error {
	if err := os.Mkdir(path, 0755); err != nil && !os.IsExist(err) {
		return errors.New("cannot create workspace")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("workspace must be a real directory")
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return errors.New("cannot inspect workspace")
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
		return errors.New("workspace must be supervisor-owned and not writable by others")
	}
	for _, name := range []string{"home", "project"} {
		if err := unix.Mkdirat(fd, name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return errors.New("cannot create actor directory")
		}
		child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return errors.New("actor directory must not be a symlink")
		}
		err = unix.Fstat(child, &st)
		if err == nil && st.Uid != uint32(uid) && st.Uid != uint32(os.Geteuid()) {
			err = errors.New("unexpected actor directory owner")
		}
		if err == nil {
			err = unix.Fchmod(child, 0700)
		}
		if err == nil {
			err = unix.Fchown(child, uid, gid)
		}
		_ = unix.Close(child)
		if err != nil {
			return errors.New("cannot secure actor directory")
		}
	}
	return nil
}

// Process must terminate all actor descendants before Close returns.
type Process interface {
	Wait() error
	Close() error
}
type actorProcess struct {
	cmd    *exec.Cmd
	socket *actorSocket
}

func (p *actorProcess) Wait() error { return p.cmd.Wait() }
func (p *actorProcess) Close() error {
	if os.Geteuid() != 0 {
		return errors.New("actor cleanup requires trusted guest root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := drainActorProcesses(ctx, sweepActorUID)
	if p.socket != nil {
		err = errors.Join(err, p.socket.Close())
	}
	return err
}
func guestHelperCommand(spec ActorSpec) (*exec.Cmd, error) {
	// Only the current trusted executable can implement the privilege gate.
	self, err := os.Executable()
	if err != nil {
		return nil, errors.New("cannot locate guest executable")
	}
	if spec.Executable != "/proc/self/exe" && filepath.Clean(spec.Executable) != self {
		return nil, errors.New("actor executable must be the supervisor itself")
	}
	file, err := os.Open("/proc/self/exe")
	if err != nil {
		return nil, errors.New("cannot inspect guest executable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("cannot inspect guest executable")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("guest executable must be root-owned and immutable to actor")
	}
	cmd := exec.Command("/proc/self/exe", spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}}, Setpgid: true, Pdeathsig: syscall.SIGKILL}
	return cmd, nil
}
func launchActor(ctx context.Context, spec ActorSpec) (Process, error) {
	if os.Geteuid() != 0 || len(spec.Args) != 1 || spec.Args[0] != "--brood-guest-actor" {
		return nil, errors.New("PTY actor launch requires the fixed supervisor target")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd, err := guestHelperCommand(spec)
	if err != nil {
		return nil, err
	}
	socket, err := createProductionPTYSocket()
	if err != nil {
		return nil, err
	}
	file, err := socket.listener.File()
	if err != nil {
		socket.Close()
		return nil, errors.New("cannot inherit PTY listener")
	}
	defer file.Close()
	cmd.ExtraFiles = []*os.File{file}
	if err := cmd.Start(); err != nil {
		socket.Close()
		return nil, fmt.Errorf("actor helper start failed: %s", safeExecError(err))
	}
	return &actorProcess{cmd: cmd, socket: socket}, nil
}
func safeExecError(err error) string {
	if errors.Is(err, os.ErrPermission) {
		return "permission denied"
	}
	if errors.Is(err, os.ErrNotExist) {
		return "executable missing"
	}
	return "execution refused"
}
