//go:build linux

package sessionctl

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
	"brood.dev/go/brood/proxmox/relay"
)

const fixedWorkspace = "/workspace/project"
const workspaceFileLimit = 100000

// Переходы делят общий бюджет 15 s, меньший внешнего helper timeout 18 s.
const workspaceStopTimeout = 4 * time.Second
const workspaceStartTimeout = 8 * time.Second
const workspacePollInterval = 100 * time.Millisecond

// Одна команда Git укладывается в общий бюджет с запасом на соседние шаги.
const workspaceStepTimeout = 4 * time.Second

type workspaceEngine struct {
	root   string
	git    func(context.Context, ...string) (string, error)
	stream func(context.Context, []string, func(*bufio.Reader) error) error
	call   ptyCall
	prove  func(context.Context) (identity, error)
	quiet  func(context.Context) (bool, error)
}

// RunWorkspace restarts the session inside the unprivileged helper. The caller
// must hold the server's workspace reservation and authorization; restart
// means a session restart in the same CT, never a PVE reset or reboot, and it
// needs no Git checkout.
func RunWorkspace(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	if err := requireActorIdentity("workspace helper"); err != nil {
		return failure(err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	e := workspaceEngine{root: fixedWorkspace, git: func(ctx context.Context, args ...string) (string, error) {
		return runWorkspaceGit(ctx, actorHome, fixedWorkspace, args...)
	}, stream: func(ctx context.Context, args []string, consume func(*bufio.Reader) error) error {
		return streamWorkspaceGit(ctx, actorHome, fixedWorkspace, args, consume)
	}, call: callPTY, prove: proveLive, quiet: workspaceQuiet}
	return e.handle(ctx, req)
}

// requireActorIdentity confirms that a workspace helper runs exactly as the
// actor, with no supplementary group that could reach root-owned state.
func requireActorIdentity(helper string) error {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return errors.New(helper + " requires UID and GID 1000")
	}
	groups, err := os.Getgroups()
	if err != nil {
		return errors.New(helper + " group check failed")
	}
	for _, g := range groups {
		if g != 1000 {
			return errors.New(helper + " has unexpected supplementary groups")
		}
	}
	return nil
}

func workspaceUnknown(text string) relay.SessionResponse {
	return relay.SessionResponse{Text: text + "; completion is unconfirmed, do not retry automatically", Outcome: "unknown"}
}

func (e *workspaceEngine) handle(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	if req.Operation != "restart" {
		return relay.SessionResponse{Text: "unsupported workspace operation", Outcome: "unsupported"}
	}
	if ctx.Err() != nil {
		return failure("restart canceled; nothing changed")
	}
	var a struct {
		Name string `json:"name"`
	}
	if decodeArgs(req.Arguments, &a) != nil || len(a.Name) > 128 || control(a.Name) {
		return failure("invalid restart arguments; only name is accepted")
	}
	checkout, err := e.require(ctx)
	if err != nil {
		return failure(err.Error())
	}
	before, err := e.prove(ctx)
	if err != nil {
		return failure("no proven live Claude session; nothing changed")
	}
	if before.HolderProof == "" {
		return failure("current PTY holder proof unavailable; nothing changed")
	}
	after, err := e.prove(ctx)
	if err != nil || before != after {
		return failure("live session changed before stop; nothing changed")
	}
	if ctx.Err() != nil {
		return failure("restart canceled before stop; nothing changed")
	}
	reply, err := e.call(ctx, ptysession.Request{Action: "stop", Proof: before.HolderProof})
	if err != nil || !reply.OK {
		if reply.Outcome == "refused" {
			return ptyFailure(reply, err, true)
		}
		return workspaceUnknown("session stop result is unknown")
	}
	// Ответ holder не заменяет независимый scan всех процессов workspace.
	if err = workspaceWait(ctx, workspaceStopTimeout, e.quiet); err != nil {
		return workspaceUnknown("session stop is not yet confirmed; workspace is live or process inventory is unknown")
	}
	if ctx.Err() != nil {
		return workspaceUnknown("session stopped; operation canceled before launch")
	}
	// Повторный scan отказывает при появлении чужой работы.
	quiet, err := e.quiet(ctx)
	if err != nil || !quiet {
		return workspaceUnknown("session stopped; workspace is live or process inventory is unknown")
	}
	if err = e.reprove(ctx, checkout); err != nil {
		return workspaceUnknown("session stopped; " + err.Error())
	}
	if reply, err = e.call(ctx, ptysession.Request{Action: "ensure"}); err != nil || !reply.OK {
		if err == nil && reply.Outcome == "refused" {
			return failure("The previous session stopped, but no new session started. Check the server and the assigned role, then stop and start the agent")
		}
		return workspaceUnknown("session launch result is unknown")
	}
	if err = workspaceWait(ctx, workspaceStartTimeout, func(ctx context.Context) (bool, error) {
		current, err := e.prove(ctx)
		return err == nil && current != before, nil
	}); err != nil {
		return workspaceUnknown("new Claude session is not yet proven")
	}
	return success(map[string]any{"session": "restarted", "workspace": "kept", "container": "unchanged"})
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

// require proves, before any effect, what restart needs of the workspace. It
// keeps the workspace and runs no Git effect (#158): the fixed directory
// suffices, as for an agent without origin; a checkout it does find is
// reported, and the launch requires it again.
func (e *workspaceEngine) require(ctx context.Context) (checkout bool, err error) {
	if e.repository(ctx) == nil {
		return true, nil
	}
	return false, fixedDirectory(e.root)
}

// reprove repeats after the stop the proof that require gave before it: the
// old session no longer runs, so a lost proof means someone else changed the
// workspace, and the operation can no longer claim to have preserved it.
func (e *workspaceEngine) reprove(ctx context.Context, checkout bool) error {
	if !checkout {
		if fixedDirectory(e.root) != nil {
			return errors.New("fixed workspace proof was lost")
		}
		return nil
	}
	if e.repository(ctx) != nil {
		return errors.New("repository proof was lost")
	}
	return nil
}

// fixedDirectory proves that workspace is a directory reached without symlinks.
func fixedDirectory(workspace string) error {
	root, err := openSafe(workspace, true)
	if err != nil {
		return errors.New("fixed workspace is unavailable or unsafe")
	}
	root.Close()
	return nil
}

// repository rejects indirection and executable Git configuration before status
// or checkout. No origin, credential helper, network, or bootstrap is invoked.
func (e *workspaceEngine) repository(ctx context.Context) error {
	if err := repositoryLayout(ctx, e.root, e.git); err != nil {
		return err
	}
	return e.stream(ctx, indexArgs, refuseGitlinks)
}

// dirty reads status as a stream: it lists every changed, untracked and
// ignored path, so it grows with the work in the clone; only emptiness counts.
func (e *workspaceEngine) dirty(ctx context.Context) (bool, error) {
	listed := int64(0)
	err := e.stream(ctx, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching"}, func(r *bufio.Reader) (err error) {
		listed, err = io.Copy(io.Discard, r)
		return err
	})
	return listed > 0, err
}

// indexArgs lists the index, one "<mode> <object> <stage>\t<path>" per NUL.
var indexArgs = []string{"ls-files", "--stage", "-z"}

// Индекс бывает длиннее рабочего дерева: стадии конфликта, staged-удаления,
// skip-worktree. Запас вдвое против бюджета файлов, как у snapshotObjectLimit.
const indexEntryLimit = 2 * workspaceFileLimit

// refuseGitlinks is the index check of workspace operations and checkout.
func refuseGitlinks(r *bufio.Reader) error { return scanIndex(r, indexEntryLimit) }

// scanIndex reads an index listing one entry at a time, so memory stays within
// the buffer of r whatever the size of the index. It refuses a gitlink, an
// entry longer than the buffer, a truncated listing and more than limit entries.
func scanIndex(r *bufio.Reader, limit int) error {
	for n := 0; ; n++ {
		entry, err := r.ReadSlice(0)
		switch {
		case err == io.EOF && len(entry) == 0:
			return nil
		case err == bufio.ErrBufferFull:
			return fmt.Errorf("Git index has an entry over %d bytes", r.Size())
		case err != nil:
			return errors.New("Git index listing is truncated")
		case n == limit:
			return fmt.Errorf("Git index has more than %d entries", limit)
		case bytes.HasPrefix(entry, []byte("160000 ")):
			return errors.New("submodule workspaces are not supported")
		}
	}
}

// repositoryLayout checks the clone before a session restart.
func repositoryLayout(ctx context.Context, workspace string, git func(context.Context, ...string) (string, error)) error {
	if err := fixedDirectory(workspace); err != nil {
		return err
	}
	gitdir, err := openSafe(filepath.Join(workspace, ".git"), true)
	if err != nil {
		return errors.New("workspace must be a Git clone with an in-place .git directory")
	}
	gitdir.Close()
	files := 0
	if err := workspaceWalk(ctx, workspace, false, 0, &files); err != nil {
		return errors.New("workspace traversal unsafe, canceled, or exceeds file limit")
	}
	for _, name := range []string{"commondir", "config.worktree", "info/grafts", "objects/info/alternates", "objects/info/http-alternates", "refs/replace"} {
		if _, err := os.Lstat(filepath.Join(workspace, ".git", name)); !os.IsNotExist(err) {
			return errors.New("indirect Git metadata is not supported")
		}
	}
	if _, err = readSafe(filepath.Join(workspace, ".git/config"), 64*1024); err != nil {
		return errors.New("Git configuration unavailable or exceeds size limit")
	}
	config, err := git(ctx, "config", "--file", filepath.Join(workspace, ".git/config"), "--no-includes", "--null", "--list")
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
	top, err := git(ctx, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(top) != workspace {
		return errors.New("Git top-level is not the fixed workspace")
	}
	actual, err := git(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil || strings.TrimSpace(actual) != filepath.Join(workspace, ".git") {
		return errors.New("Git directory escapes the fixed workspace")
	}
	if _, err = git(ctx, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return errors.New("workspace HEAD is not a local commit")
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

// brood.origin — канонический origin, записанный первым клоном (#147); это
// данные, а не исполняемая настройка Git. user.name и user.email — тоже
// данные.
func workspaceConfigKey(key string) bool {
	switch key {
	case "core.repositoryformatversion", "core.filemode", "core.bare", "core.logallrefupdates", "core.ignorecase", "core.precomposeunicode", "remote.origin.url", "remote.origin.fetch", "user.name", "user.email", "brood.origin":
		return true
	}
	return strings.HasPrefix(key, "branch.") && (strings.HasSuffix(key, ".remote") || strings.HasSuffix(key, ".merge"))
}

// actorHome is HOME of every helper Git; Git reads no configuration from it.
const actorHome = "/workspace/home"

// gitEnv is the one environment of helper Git in the actor-owned clone: no
// system or global configuration or attributes, prompts, protocols, replace
// objects or optional locks. home is a parameter only so that tests can plant
// a hostile global configuration where Git would otherwise read it.
func gitEnv(home, root string) []string {
	return []string{"HOME=" + home, "PWD=" + root, "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_ALLOW_PROTOCOL=", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0"}
}

// gitFlags is the one set of overrides of helper Git: every setting that
// could run code, hide changes or start background work. Excludes and
// attributes files default to paths under HOME, which GIT_CONFIG_GLOBAL does
// not disable. Repository configuration is allowlisted by workspaceConfigKey.
func gitFlags() []string {
	return []string{
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null",
		"-c", "core.excludesFile=/dev/null", "-c", "credential.helper=", "-c", "core.askPass=/bin/false",
		"-c", "submodule.recurse=false", "-c", "core.untrackedCache=false", "-c", "core.sparseCheckout=false",
		"-c", "core.fileMode=true", "-c", "core.symlinks=true", "-c", "core.ignoreCase=false",
		"-c", "core.precomposeUnicode=false", "-c", "core.autocrlf=false", "-c", "core.checkStat=default",
		"-c", "core.trustctime=true", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"-c", "fetch.writeCommitGraph=false", "-c", "advice.detachedHead=false", "-c", "protocol.allow=never",
	}
}

func runWorkspaceGit(ctx context.Context, home, root string, args ...string) (string, error) {
	return helperGit{home: home, root: root}.run(ctx, args...)
}

func streamWorkspaceGit(ctx context.Context, home, root string, args []string, consume func(*bufio.Reader) error) error {
	return helperGit{home: home, root: root}.stream(ctx, args, consume)
}

// helperGit runs helper Git in the clone at root with gitEnv and gitFlags,
// each command in its own process group, killed with its context.
type helperGit struct {
	home, root string
}

func (g helperGit) command(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/usr/bin/git", append(gitFlags(), args...)...)
	cmd.Dir = g.root
	cmd.Env = gitEnv(g.home, g.root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	return cmd
}

// run returns the output of one bounded command, refused past the cap of
// cappedOutput. Listings that grow with the clone go through stream instead.
func (g helperGit) run(parent context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, workspaceStepTimeout)
	defer cancel()
	cmd := g.command(ctx, args)
	output := &cappedOutput{cancel: cancel}
	cmd.Stdout = output
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return output.String(), err
}

// stream hands stdout to consume; an early consumer error stops Git.
func (g helperGit) stream(parent context.Context, args []string, consume func(*bufio.Reader) error) error {
	ctx, cancel := context.WithTimeout(parent, workspaceStepTimeout)
	defer cancel()
	cmd := g.command(ctx, args)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("Git output unavailable")
	}
	if err := cmd.Start(); err != nil {
		return errors.New("Git unavailable")
	}
	consumed := consume(bufio.NewReaderSize(stdout, 64<<10))
	if consumed != nil {
		cancel()
	}
	waited := cmd.Wait()
	if consumed != nil {
		return consumed
	}
	if ctx.Err() != nil {
		return errors.New("Git timed out")
	}
	if waited != nil {
		return errors.New("Git failed")
	}
	return nil
}

// EnsureSession asks the trusted PTY holder to ensure its fixed command exists.
// The helper never launches a process or replaces an existing child itself.
func EnsureSession(ctx context.Context) error { return ensureSession(ctx, callPTY) }

func ensureSession(ctx context.Context, call ptyCall) error {
	reply, err := call(ctx, ptysession.Request{Action: "ensure"})
	if err != nil || !reply.OK || !reply.State.Running || reply.Proof == "" {
		return errors.New("fixed Claude PTY session could not be ensured; completion is unconfirmed")
	}
	return nil
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
