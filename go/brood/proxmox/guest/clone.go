//go:build linux

package guest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"brood.dev/go/brood/gitidentity"
)

// Маркер переживает прерванный wrapper, но не доказывает происхождение файлов:
// другая сессия могла записать работу до следующего старта.
const cloneMarker = "workspace-clone.pending"
const cloneLock = "workspace-clone.lock"

type cloneGit func(ctx context.Context, env []string, args ...string) error

// cloneSpec fixes every input of the first-start clone; nothing comes from the actor.
type cloneSpec struct {
	Project, Home string
	Origin        string
	// Identity is the installation-assigned commit identity (#249); zero
	// leaves the clone without user.name and user.email, as before.
	Identity gitidentity.Identity
	Timeout  time.Duration
	Git      cloneGit // nil: /usr/bin/git
}

func cloneFailure(reason string) error { return errors.New("workspace clone failed: " + reason) }

// cloneWorkspace clones only an empty workspace. Contents are preserved after
// failure or interruption; recovery must never delete possible user work.
func cloneWorkspace(parent context.Context, s cloneSpec) error {
	if s.Origin == "" {
		return nil
	}
	if err := checkWorkspaceOrigin(WorkspaceOrigin{Origin: s.Origin, GitIdentity: s.Identity}); err != nil {
		return cloneFailure(err.Error())
	}
	git := s.Git
	if git == nil {
		git = runCloneGit
	}
	state, err := secretManifestDirectory(s.Home)
	if err != nil {
		return cloneFailure("private state directory is unavailable or unsafe")
	}
	defer unix.Close(state)
	lock, err := lockPrivateFile(state, cloneLock)
	if err != nil {
		return cloneFailure("clone lock is unavailable or unsafe")
	}
	defer unix.Close(lock)
	empty, err := emptyWorkspace(s.Project)
	if err != nil {
		return cloneFailure("workspace is unavailable or unsafe")
	}
	if !empty {
		if !privateFileExists(state, cloneMarker) {
			return nil
		}
		return cloneFailure("interrupted clone; workspace preserved, explicit reset required")
	}
	if replaceSecretFile(state, cloneMarker, []byte(s.Origin+"\n"), 0600, os.Geteuid(), os.Getegid(), false) != nil {
		return cloneFailure("cannot record the pending clone")
	}
	// Git сам удаляет destination при ошибке. FD родителя удерживает staging
	// вне живой рабочей копии даже при переименовании домашнего каталога.
	staging, err := os.MkdirTemp(fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), state), "workspace-clone-")
	if err != nil {
		return cloneFailure("cannot create an isolated clone directory")
	}
	defer os.RemoveAll(staging)
	ctx, cancel := context.WithTimeout(parent, s.Timeout)
	defer cancel()
	protocol, _, _ := strings.Cut(s.Origin, ":")
	env := cloneEnv(s.Home, protocol)
	fixed := []string{"-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "core.hooksPath=/dev/null"}
	cloneErr := git(ctx, env, append(fixed, "clone", "--", s.Origin, staging)...)
	if cloneErr != nil {
		cloneErr = cloneFailure(cloneErr.Error())
	} else if git(ctx, env, append(fixed, "-C", staging, "config", "brood.origin", s.Origin)...) != nil {
		cloneErr = cloneFailure("cannot record brood.origin")
	} else if err := recordIdentity(ctx, git, env, fixed, staging, s.Identity); err != nil {
		cloneErr = err
	} else if err := publishClone(state, filepath.Base(staging), s.Project); err != nil {
		// Частичная публикация не даёт права повторять клон поверх работы.
		return cloneFailure("cannot publish clone; workspace preserved, explicit reset required")
	}
	// Дальше стартует Claude: его работа не должна выглядеть остатком клона.
	if unix.Unlinkat(state, cloneMarker, 0) != nil || unix.Fsync(state) != nil {
		return cloneFailure("cannot clear the pending clone record")
	}
	return cloneErr
}

// recordIdentity writes the actor identity into the staged clone's own
// .git/config (#249): the actor's HOME carries no Git configuration, and the
// helper Git of workspace operations reads none from it.
func recordIdentity(ctx context.Context, git cloneGit, env, fixed []string, staging string, identity gitidentity.Identity) error {
	if identity.IsZero() {
		return nil
	}
	// Срез, а не map: порядок записи фиксирован, тесты и журнал его видят.
	for _, kv := range [][2]string{{"user.name", identity.Name}, {"user.email", identity.Email}} {
		if git(ctx, env, append(fixed, "-C", staging, "config", kv[0], kv[1])...) != nil {
			return cloneFailure("cannot record the actor Git identity")
		}
	}
	return nil
}

// publishClone never replaces a live entry. A concurrent writer may interrupt
// publication, but cannot lose a file; .git becomes visible only at the end.
func publishClone(state int, staging, project string) error {
	source, err := unix.Openat(state, staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	src := os.NewFile(uintptr(source), "clone staging")
	defer src.Close()
	target, err := openSecretDirectory(project, false)
	if err != nil {
		return err
	}
	dst := os.NewFile(uintptr(target), "clone workspace")
	defer dst.Close()
	if err := secureSecretDirectory(target, os.Geteuid(), false); err != nil {
		return err
	}
	if names, err := dst.Readdirnames(1); err != io.EOF || len(names) != 0 {
		return errors.New("workspace is no longer empty")
	}
	names, err := src.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == ".git" {
			continue
		}
		if err := unix.Renameat2(source, name, target, name, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
	}
	if err := unix.Renameat2(source, ".git", target, ".git", unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	return unix.Fsync(target)
}

func cloneEnv(home, protocol string) []string {
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_ALLOW_PROTOCOL=" + protocol}
	if protocol == "ssh" {
		// Клон не читает пользовательскую конфигурацию и не открывает запросы в PTY.
		env = append(env, "GIT_SSH_COMMAND="+sshClient+" -F /dev/null -o BatchMode=yes -o IdentityFile="+workspaceSSHKeyPath+
			" -o IdentitiesOnly=yes -o IdentityAgent=none -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="+sshKnownHosts+" -o UpdateHostKeys=no")
	}
	return env
}

func emptyWorkspace(path string) (bool, error) {
	fd, err := openSecretDirectory(path, false)
	if err != nil {
		return false, err
	}
	dir := os.NewFile(uintptr(fd), "workspace")
	defer dir.Close()
	if err := secureSecretDirectory(fd, os.Geteuid(), false); err != nil {
		return false, err
	}
	names, err := dir.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return len(names) == 0, err
}

func privateFileExists(dir int, name string) bool {
	var st unix.Stat_t
	return unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil
}

// runCloneGit stops Git with SIGTERM on timeout, so it removes its partial clone.
func runCloneGit(ctx context.Context, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/git", args...)
	cmd.Env = env
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	stderr := &tailBuffer{limit: 4 << 10}
	cmd.Stdout, cmd.Stderr = io.Discard, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return errors.New("git timed out")
	}
	if err != nil {
		// Git и SSH могут вывести произвольный текст удалённой стороны.
		diagnostics := string(stderr.Bytes())
		switch {
		case strings.Contains(diagnostics, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
			return errors.New("SSH host key changed; verify the Git host key before updating known_hosts")
		case strings.Contains(diagnostics, "Host key verification failed"):
			return errors.New("SSH host key verification failed; check the Git host and known_hosts")
		case strings.Contains(diagnostics, "Permission denied (publickey"):
			return errors.New("SSH authentication failed; authorize the swarm public key on the Git host")
		case strings.Contains(diagnostics, "could not resolve host") || strings.Contains(diagnostics, "Could not resolve hostname"):
			return errors.New("Git host name could not be resolved; check the origin and DNS")
		default:
			return errors.New("Git could not clone the repository; check the origin, connectivity and access")
		}
	}
	return nil
}

// tailBuffer keeps only the last bytes of Git's diagnostics. The buffer is a
// field, not embedded: io.Copy would take a promoted ReadFrom over Write.
type tailBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf.Write(p)
	if extra := b.buf.Len() - b.limit; extra > 0 {
		b.buf.Next(extra)
	}
	return len(p), nil
}

func (b *tailBuffer) Bytes() []byte { return b.buf.Bytes() }

// gitReason is the last diagnostic line as printable ASCII of at most 300 bytes.
func gitReason(stderr []byte) string {
	lines := strings.FieldsFunc(string(stderr), func(r rune) bool { return r == '\n' || r == '\r' })
	reason := ""
	for i := len(lines) - 1; i >= 0 && reason == ""; i-- {
		reason = strings.TrimSpace(strings.Map(func(r rune) rune {
			if r < 0x20 || r > 0x7e {
				return -1
			}
			return r
		}, lines[i]))
	}
	if reason == "" {
		return "git exited unsuccessfully"
	}
	if len(reason) > 300 {
		reason = reason[:300]
	}
	return reason
}
