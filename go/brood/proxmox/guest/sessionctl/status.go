//go:build linux

package sessionctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"brood.dev/go/brood/proxmox/guest/ccmsg"
	"brood.dev/go/brood/proxmox/guest/ptysession"
	"brood.dev/go/brood/proxmox/relay"
)

// Обмер клона делит один бюджет, меньший бюджета helper (15 s): медленный
// Git отдаёт «не знаю», а не съедает ответ о сессии.
const statusCloneTimeout = 6 * time.Second

// status reports what the session does and what the clone holds. It changes
// nothing and needs no proof for its answer: a session it cannot prove is
// reported as offline, a clone it cannot read as unknown.
func (e *engine) status(ctx context.Context, raw []byte) relay.SessionResponse {
	var a struct{}
	if decodeArgs(raw, &a) != nil {
		return failure("status takes no arguments")
	}
	status := relay.AgentStatus{Model: savedModel(e.config)}
	status.Session, status.Error = e.sessionState(ctx)
	cloneCtx, cancel := context.WithTimeout(ctx, statusCloneTimeout)
	defer cancel()
	status.Clone, status.Branch, status.Uncommitted, status.Unpushed = cloneFacts(cloneCtx, helperGit{home: actorHome, root: fixedWorkspace})
	return success(status)
}

// sessionState соединяет файл активности Claude Code с ходом, который ведёт
// PTY-хост по хукам: файл знает busy и idle, но не знает, что ход упал.
func (e *engine) sessionState(ctx context.Context) (state, code string) {
	id, err := e.prove(ctx)
	if err != nil {
		return relay.SessionOffline, ""
	}
	reply, err := e.call(ctx, ptysession.Request{Action: "status"})
	turn := ""
	if err == nil && reply.OK {
		turn = reply.Turn
	}
	_, activity, _ := ccmsg.ReadSessionActivity(id.PID)
	switch {
	case activity == "busy" || activity == "shell":
		return relay.SessionBusy, ""
	case turn == "error":
		return relay.SessionError, ptysession.SessionErrorCode(reply.TurnError)
	case activity == "requires_action" || turn == "paused":
		return relay.SessionNeedsAction, ""
	case activity == "waiting":
		return relay.SessionWaiting, ""
	case turn == "working":
		// Файл активности отстаёт от хука на старте хода.
		return relay.SessionBusy, ""
	case activity == "idle":
		return relay.SessionIdle, ""
	}
	// Незнакомый статус — не повод считать сессию свободной.
	return relay.SessionUnknown, ""
}

// savedModel reads the model Claude Code keeps in its user settings: the one
// seeded from the role, or the one chosen later with /model.
func savedModel(config string) string {
	data, err := readSafe(filepath.Join(config, "settings.json"), 256*1024)
	if err != nil {
		return ""
	}
	var settings struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return ""
	}
	return statusText(settings.Model)
}

// statusText keeps one printable line of bounded length, or nothing.
func statusText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 200 || control(value) {
		return ""
	}
	return value
}

// cloneFacts measures the clone with helper Git: the branch, the changed and
// untracked paths, and the commits no remote branch has. A workspace without
// .git is CloneNone; any Git that did not answer is CloneUnknown, never zero.
func cloneFacts(ctx context.Context, git helperGit) (clone, branch string, uncommitted, unpushed int) {
	if fixedDirectory(git.root) != nil {
		return relay.CloneUnknown, "", 0, 0
	}
	if _, err := os.Lstat(filepath.Join(git.root, ".git")); os.IsNotExist(err) {
		return relay.CloneNone, "", 0, 0
	}
	unknown := func() (string, string, int, int) { return relay.CloneUnknown, "", 0, 0 }
	born := true
	if _, err := git.run(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		if ctx.Err() != nil {
			return unknown()
		}
		born = false
	}
	name, err := git.run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	branch = statusText(name)
	if err != nil || branch == "" {
		// Отсоединённый HEAD: веткой называется сам коммит.
		commit, err := git.run(ctx, "rev-parse", "--short", "HEAD")
		if err != nil || statusText(commit) == "" {
			return unknown()
		}
		branch = "detached at " + statusText(commit)
	}
	err = git.stream(ctx, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames"}, func(r *bufio.Reader) error {
		buffer := make([]byte, 32<<10)
		for {
			n, err := r.Read(buffer)
			uncommitted += bytes.Count(buffer[:n], []byte{0})
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	})
	if err != nil {
		return unknown()
	}
	if !born {
		return relay.CloneGit, branch, uncommitted, 0
	}
	// Без единой удалённой ветки отправлять некуда: каждый коммит числился
	// бы неотправленным, и агент без origin не был бы свободен никогда.
	remotes, err := git.run(ctx, "for-each-ref", "--count=1", "--format=%(refname)", "refs/remotes")
	if err != nil {
		return unknown()
	}
	if strings.TrimSpace(remotes) == "" {
		return relay.CloneGit, branch, uncommitted, 0
	}
	// `--not --remotes`, а не `@{u}..`: upstream рабочей ветки бывает прибит
	// к основной, и тогда всё невлитое считалось бы неотправленным.
	count, err := git.run(ctx, "rev-list", "--count", "HEAD", "--not", "--remotes")
	if err != nil {
		return unknown()
	}
	if unpushed, err = strconv.Atoi(strings.TrimSpace(count)); err != nil || unpushed < 0 {
		return unknown()
	}
	return relay.CloneGit, branch, uncommitted, unpushed
}
