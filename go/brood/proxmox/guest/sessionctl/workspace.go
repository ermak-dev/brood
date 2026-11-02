//go:build linux

package sessionctl

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

const fixedWorkspace = "/workspace/project"
const workspaceFileLimit = 100000

// Переходы делят общий бюджет 15 s, меньший внешнего helper timeout 18 s.
const workspaceStopTimeout = 4 * time.Second
const workspaceStartTimeout = 8 * time.Second
const workspacePollInterval = 100 * time.Millisecond

type workspaceEngine struct {
	root  string
	git   func(context.Context, ...string) (string, error)
	tmux  func(context.Context, ...string) (string, error)
	prove func(context.Context) (identity, error)
	quiet func(context.Context) (bool, error)
}

type workspaceFacts struct {
	Path           string `json:"path"`
	Git            string `json:"git"`
	Reason         string `json:"reason,omitempty"`
	SessionRunning bool   `json:"session_running"`
}

// RunWorkspace performs fixed-clone operations inside the unprivileged helper.
// The caller must hold the server's actor operation lease and authorization;
// restart means a session restart in the same CT, never a PVE reset or reboot.
func RunWorkspace(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return failure("workspace helper requires UID and GID 1000")
	}
	groups, err := os.Getgroups()
	if err != nil {
		return failure("workspace helper group check failed")
	}
	for _, g := range groups {
		if g != 1000 {
			return failure("workspace helper has unexpected supplementary groups")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	e := workspaceEngine{root: fixedWorkspace, git: func(ctx context.Context, args ...string) (string, error) {
		return runWorkspaceGit(ctx, fixedWorkspace, args...)
	}, tmux: runWorkspaceTMUX, prove: proveLive, quiet: workspaceQuiet}
	return e.handle(ctx, req)
}

func workspaceUnknown(text string) relay.SessionResponse {
	return relay.SessionResponse{Text: text + "; completion is unconfirmed, do not retry automatically", Outcome: "unknown"}
}

func (e *workspaceEngine) handle(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	if req.Operation != "workspace" && req.Operation != "wipe" && req.Operation != "recycle" && req.Operation != "restart" {
		return relay.SessionResponse{Text: "unsupported workspace operation", Outcome: "unsupported"}
	}
	if ctx.Err() != nil {
		return failure("workspace operation canceled")
	}
	var a struct {
		Name    string `json:"name"`
		Confirm bool   `json:"confirm"`
	}
	if decodeArgs(req.Arguments, &a) != nil || len(a.Name) > 128 || control(a.Name) {
		return failure("invalid workspace arguments; only name and confirm are accepted")
	}
	if req.Operation == "workspace" {
		facts := workspaceFacts{Path: fixedWorkspace, Git: "unknown"}
		if err := e.repository(ctx); err != nil {
			facts.Reason = err.Error()
		} else if status, err := e.git(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching"); err != nil {
			facts.Reason = "Git status unavailable or exceeds output limit"
		} else if status != "" {
			facts.Git = "dirty"
		} else {
			facts.Git = "clean"
		}
		_, err := e.prove(ctx)
		facts.SessionRunning = err == nil
		return success(facts)
	}
	if !a.Confirm {
		return failure("workspace operation requires confirm=true")
	}
	if err := e.repository(ctx); err != nil {
		return failure(err.Error())
	}
	if req.Operation != "wipe" {
		before, err := e.prove(ctx)
		if err != nil {
			return failure("no proven live Claude session; nothing changed")
		}
		panes, err := e.tmux(ctx, "list-panes", "-s", "-t", "=claude", "-F", "#{session_id}\t#{pane_id}")
		row := strings.Split(strings.TrimSuffix(panes, "\n"), "\t")
		if err != nil || len(row) != 2 || !workspaceTMUXID(row[0], '$') || !workspaceTMUXID(row[1], '%') {
			return failure("fixed tmux session must have exactly one proven pane; nothing changed")
		}
		after, err := e.prove(ctx)
		if err != nil || before != after {
			return failure("live session changed before stop; nothing changed")
		}
		if ctx.Err() != nil {
			return failure("workspace operation canceled before stop")
		}
		if _, err = e.tmux(ctx, "kill-session", "-t", row[0]); err != nil {
			return workspaceUnknown("session stop result is unknown")
		}
		// kill-session подтверждает команду, но не завершение дерева процессов.
		if err = workspaceWait(ctx, workspaceStopTimeout, e.quiet); err != nil {
			return workspaceUnknown("session stop is not yet confirmed; workspace is live or process inventory is unknown")
		}
		if req.Operation == "recycle" {
			if err := e.clean(ctx); err != nil {
				return workspaceUnknown("session stopped; " + err.Error())
			}
		}
		if ctx.Err() != nil {
			return workspaceUnknown("session stopped; operation canceled before launch")
		}
		// Повторный scan не заменяет lease: он отказывает при появлении чужой работы.
		quiet, err := e.quiet(ctx)
		if err != nil || !quiet {
			return workspaceUnknown("session stopped; workspace is live or process inventory is unknown")
		}
		if err = e.repository(ctx); err != nil {
			return workspaceUnknown("session stopped; repository proof was lost")
		}
		if _, err = e.tmux(ctx, workspaceLaunchArgs()...); err != nil {
			return workspaceUnknown("session launch result is unknown")
		}
		if err = workspaceWait(ctx, workspaceStartTimeout, func(ctx context.Context) (bool, error) {
			current, err := e.prove(ctx)
			return err == nil && current != before, nil
		}); err != nil {
			return workspaceUnknown("new Claude session is not yet proven")
		}
		return success(map[string]any{"operation": req.Operation, "session": "restarted", "workspace_preserved": req.Operation == "restart", "ct": "unchanged"})
	}
	if err := e.clean(ctx); err != nil {
		var effect *workspaceEffectError
		if errors.As(err, &effect) {
			return workspaceUnknown(err.Error())
		}
		return failure(err.Error())
	}
	return success(map[string]any{"operation": "wipe", "git": "clean", "clone_preserved": true, "ct": "unchanged"})
}

// workspaceWait polls evidence only, never effects. Inventory errors fail closed;
// callers may treat absent session proof as pending, but never as success.
func workspaceWait(parent context.Context, budget time.Duration, ready func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	ticker := time.NewTicker(workspacePollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := ready(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type workspaceEffectError struct{ text string }

func (e *workspaceEffectError) Error() string { return e.text }

func (e *workspaceEngine) clean(ctx context.Context) error {
	quiet, err := e.quiet(ctx)
	if err != nil || !quiet {
		return errors.New("workspace has live processes or process inventory is unknown; cleanup refused")
	}
	if err = e.repository(ctx); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errors.New("workspace operation canceled before cleanup")
	}
	if _, err = e.git(ctx, "reset", "--hard", "HEAD", "--"); err != nil {
		return &workspaceEffectError{"Git reset result is unknown"}
	}
	// После первого эффекта любой отказ означает частично выполненную операцию.
	quiet, err = e.quiet(ctx)
	if err != nil || !quiet || ctx.Err() != nil {
		return &workspaceEffectError{"workspace changed or operation canceled after reset"}
	}
	if err = e.repository(ctx); err != nil {
		return &workspaceEffectError{"repository proof lost after reset"}
	}
	if _, err = e.git(ctx, "clean", "-ffdx", "--"); err != nil {
		return &workspaceEffectError{"Git cleanup result is unknown"}
	}
	status, err := e.git(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil || status != "" || ctx.Err() != nil {
		return &workspaceEffectError{"workspace cleanliness is not confirmed after cleanup"}
	}
	return nil
}

// repository rejects indirection and executable Git configuration before status
// or checkout. No origin, credential helper, network, or bootstrap is invoked.
func (e *workspaceEngine) repository(ctx context.Context) error {
	root, err := openSafe(e.root, true)
	if err != nil {
		return errors.New("fixed workspace is unavailable or unsafe")
	}
	root.Close()
	gitdir, err := openSafe(filepath.Join(e.root, ".git"), true)
	if err != nil {
		return errors.New("workspace must be a Git clone with an in-place .git directory")
	}
	gitdir.Close()
	files := 0
	if err = workspaceWalk(ctx, e.root, false, 0, &files); err != nil {
		return errors.New("workspace traversal unsafe, canceled, or exceeds file limit")
	}
	for _, name := range []string{"commondir", "config.worktree", "info/grafts", "objects/info/alternates", "objects/info/http-alternates", "refs/replace"} {
		if _, err := os.Lstat(filepath.Join(e.root, ".git", name)); !os.IsNotExist(err) {
			return errors.New("indirect Git metadata is not supported")
		}
	}
	if _, err = readSafe(filepath.Join(e.root, ".git/config"), 64*1024); err != nil {
		return errors.New("Git configuration unavailable or exceeds size limit")
	}
	config, err := e.git(ctx, "config", "--file", filepath.Join(e.root, ".git/config"), "--no-includes", "--null", "--list")
	if err != nil {
		return errors.New("Git configuration cannot be inspected")
	}
	for _, entry := range strings.Split(config, "\x00") {
		if entry == "" {
			continue
		}
		key, _, ok := strings.Cut(entry, "\n")
		if !ok || !workspaceConfigKey(key) {
			return errors.New("unsafe or unsupported repository Git configuration")
		}
	}
	top, err := e.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(top) != e.root {
		return errors.New("Git top-level is not the fixed workspace")
	}
	actual, err := e.git(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil || strings.TrimSpace(actual) != filepath.Join(e.root, ".git") {
		return errors.New("Git directory escapes the fixed workspace")
	}
	if _, err = e.git(ctx, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return errors.New("workspace HEAD is not a local commit")
	}
	index, err := e.git(ctx, "ls-files", "--stage", "-z")
	if err != nil {
		return errors.New("Git index unavailable or exceeds output limit")
	}
	for _, entry := range strings.Split(index, "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			return errors.New("submodule workspaces are not supported")
		}
	}
	return nil
}

// workspaceWalk reads directories in bounded batches instead of materializing
// an actor-controlled directory listing before checking the file budget.
func workspaceWalk(ctx context.Context, path string, metadata bool, depth int, files *int) error {
	if depth > 128 {
		return errors.New("workspace depth exceeds limit")
	}
	dir, err := openSafe(path, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		entries, err := dir.ReadDir(256)
		if err != nil && err != io.EOF {
			return err
		}
		for _, entry := range entries {
			*files++
			if *files > workspaceFileLimit {
				return errors.New("workspace file count exceeds limit")
			}
			childMetadata := metadata || (depth == 0 && entry.Name() == ".git")
			if childMetadata && (entry.Type()&os.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular())) {
				return errors.New("unsafe Git metadata")
			}
			if entry.IsDir() {
				if err := workspaceWalk(ctx, filepath.Join(path, entry.Name()), childMetadata, depth+1, files); err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

func workspaceConfigKey(key string) bool {
	switch key {
	case "core.repositoryformatversion", "core.filemode", "core.bare", "core.logallrefupdates", "core.ignorecase", "core.precomposeunicode", "remote.origin.url", "remote.origin.fetch", "user.name", "user.email":
		return true
	}
	return strings.HasPrefix(key, "branch.") && (strings.HasSuffix(key, ".remote") || strings.HasSuffix(key, ".merge"))
}

func workspaceGitEnv() []string {
	return []string{"HOME=/workspace/home", "PWD=/workspace/project", "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_ALLOW_PROTOCOL=", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0"}
}
func runWorkspaceGit(ctx context.Context, root string, args ...string) (string, error) {
	fixed := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null", "-c", "credential.helper=", "-c", "core.askPass=/bin/false", "-c", "protocol.allow=never", "-c", "submodule.recurse=false", "-c", "core.untrackedCache=false"}
	return workspaceExec(ctx, root, workspaceGitEnv(), "/usr/bin/git", append(fixed, args...)...)
}
func workspaceExec(parent context.Context, root string, env []string, exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = root
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	output := &cappedOutput{cancel: cancel}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return output.String(), err
}
func workspaceTMUXID(s string, prefix byte) bool {
	if len(s) < 2 || len(s) > 20 || s[0] != prefix {
		return false
	}
	for _, ch := range s[1:] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func workspaceLaunchArgs() []string {
	return []string{"new-session", "-d", "-s", "claude", "-c", fixedWorkspace, "-e", "HOME=/workspace/home", "-e", "PWD=/workspace/project", "/usr/local/bin/brood-guest", "--brood-guest-app"}
}
func runWorkspaceTMUX(ctx context.Context, args ...string) (string, error) {
	// Helper env задаёт root supervisor, не RPC. Передаём его также через -e:
	// уже живущий tmux server иначе оставит старое окружение в новой сессии.
	env := os.Environ()
	if len(args) > 0 && args[0] == "new-session" {
		launch := []string{"new-session"}
		for _, item := range env {
			launch = append(launch, "-e", item)
		}
		args = append(launch, args[1:]...)
	}
	// Новый tmux server наследует cwd клиента; он не должен удерживать clone live.
	return workspaceExec(ctx, "/workspace/home", env, "/usr/bin/tmux", args...)
}

// workspaceQuiet deliberately refuses any other process rooted in the clone,
// not just recognized Claude executables. Missing process evidence fails closed.
func workspaceQuiet(ctx context.Context) (bool, error) {
	proc, err := os.Open("/proc")
	if err != nil {
		return false, err
	}
	defer proc.Close()
	entries, err := proc.ReadDir(32769)
	if err != nil && err != io.EOF {
		return false, err
	}
	if len(entries) > 32768 {
		return false, errors.New("process inventory exceeds limit")
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		base := "/proc/" + entry.Name()
		st, err := os.Stat(base)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		owner, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			return false, errors.New("process ownership unavailable")
		}
		if owner.Uid != uint32(os.Getuid()) {
			continue
		}
		cwd, err := os.Readlink(base + "/cwd")
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if cwd == fixedWorkspace || strings.HasPrefix(cwd, fixedWorkspace+"/") || cwd == fixedWorkspace+" (deleted)" {
			return false, nil
		}
	}
	return true, nil
}
