//go:build linux

package ptysession

import (
	"crypto/rand"
	"os"
	"syscall"
	"time"
)

const SessionEpochEnv = "BROOD_SESSION_EPOCH"
const sessionHeartbeat = 6 * time.Second

// SessionHook is the local, already-filtered subset of a Claude Code hook.
type SessionHook struct {
	Epoch     string `json:"epoch"`
	SessionID string `json:"session_id"`
	Signal    string `json:"signal"`
	ErrorCode string `json:"error_code,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
}

// SessionEvent is the wire format for the supervisor's authenticated relay.
type SessionEvent struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	State     string `json:"state"`
	ErrorCode string `json:"error_code,omitempty"`
}

// ValidSessionID accepts the identifier alphabet shared with the server.
func ValidSessionID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// SessionErrorCode maps unknown or missing Claude API error categories to a
// stable fallback. Never return a provider message or a tool failure string.
func SessionErrorCode(code string) string {
	switch code {
	case "rate_limit", "authentication_failed", "oauth_org_not_allowed", "account_on_hold",
		"billing_error", "invalid_request", "model_not_found", "server_error", "max_output_tokens",
		"cloud_credential_error", "permission_denied", "launch_failed", "process_exit":
		return code
	default:
		return "unknown"
	}
}

type sessionTurn struct {
	sessionID, turnID  string
	state              string
	pauseTool, pauseID string
	nextHeartbeat      time.Time
	pendingEscape      time.Time
}

func (h *Host) sessionHookLocked(hook *SessionHook) {
	p := h.current
	if h.config.OnSessionEvent == nil || p == nil || p.exitErr != nil || p.stopping || !p.state.Running || hook == nil ||
		hook.Epoch != p.state.Epoch || !ValidSessionID(hook.SessionID) {
		return
	}
	t := &p.turn
	switch hook.Signal {
	case "start", "prompt":
		if t.sessionID != hook.SessionID {
			h.endTurnLocked(t)
			*t = sessionTurn{sessionID: hook.SessionID}
		}
		if hook.Signal == "prompt" {
			*t = sessionTurn{sessionID: hook.SessionID, turnID: rand.Text()}
			h.emitSessionStateLocked(t, "working", "")
		}
	case "failure":
		if t.sessionID == "" {
			t.sessionID = hook.SessionID
		}
		if t.sessionID != hook.SessionID || t.state == "error" {
			return
		}
		if t.turnID == "" {
			t.turnID = rand.Text()
		}
		h.emitSessionStateLocked(t, "error", SessionErrorCode(hook.ErrorCode))
	default:
		if t.sessionID != hook.SessionID || t.turnID == "" {
			return
		}
		switch hook.Signal {
		case "pause":
			if t.state == "working" {
				h.emitSessionStateLocked(t, "idle", "")
				t.state = "paused"
				t.pauseTool, t.pauseID = hook.ToolName, hook.ToolUseID
			}
		case "resume":
			// Результат соседнего параллельного инструмента не закрывает диалог.
			if t.state == "paused" && (t.pauseTool == "" || t.pauseTool == hook.ToolName) &&
				(t.pauseID == "" || t.pauseID == hook.ToolUseID) {
				h.emitSessionStateLocked(t, "working", "")
			}
		case "idle":
			h.endTurnLocked(t)
		}
	}
}

func (h *Host) emitSessionStateLocked(t *sessionTurn, state, code string) {
	t.state = state
	if state == "working" {
		t.nextHeartbeat = time.Now().Add(sessionHeartbeat)
	}
	h.config.OnSessionEvent(SessionEvent{SessionID: t.sessionID, TurnID: t.turnID, State: state, ErrorCode: code})
}

func (h *Host) endTurnLocked(t *sessionTurn) {
	if t.state == "working" {
		h.emitSessionStateLocked(t, "idle", "")
	} else if t.state == "paused" {
		// Уже передали idle на паузе. Не возобновлять этот ход после конца.
		t.state = "idle"
	}
}

func (h *Host) sessionInputLocked(p *child, data []byte) {
	if h.config.OnSessionEvent == nil || (p.turn.state != "working" && p.turn.state != "paused") {
		return
	}
	p.turn.pendingEscape = time.Time{}
	if len(data) != 1 {
		return
	}
	switch data[0] {
	case 3:
		h.endTurnLocked(&p.turn)
	case 27:
		// Escape может быть первым байтом ещё не доставленной управляющей последовательности.
		p.turn.pendingEscape = time.Now().Add(time.Second)
	}
}

func (h *Host) sessionHeartbeatLocked() {
	p := h.current
	if h.closing || p == nil || p.stopping || p.exitErr != nil || !p.state.Running {
		return
	}
	now := time.Now()
	if !p.turn.pendingEscape.IsZero() && !now.Before(p.turn.pendingEscape) {
		p.turn.pendingEscape = time.Time{}
		h.endTurnLocked(&p.turn)
	}
	if p.turn.state == "working" && !now.Before(p.turn.nextHeartbeat) {
		h.emitSessionStateLocked(&p.turn, "working", "")
	}
}

func (h *Host) sessionLaunchFailedLocked() {
	// Автостарт повторяет неудачную подготовку; это один сбой до старта
	// следующего процесса, а не ошибка на каждой попытке.
	if h.config.OnSessionEvent != nil && !h.launchFailed {
		h.launchFailed = true
		t := sessionTurn{sessionID: rand.Text(), turnID: rand.Text()}
		h.emitSessionStateLocked(&t, "error", "launch_failed")
	}
}

func (h *Host) sessionExitedLocked(p *child, status *os.ProcessState) {
	if h.config.OnSessionEvent == nil {
		return
	}
	t := &p.turn
	if !h.closing && !p.stopping && t.state != "error" && unexpectedSessionExit(status) {
		code := "process_exit"
		if t.sessionID == "" {
			t.sessionID = p.state.Epoch
			if status.ExitCode() >= 0 {
				code = "launch_failed"
			}
		}
		if t.state != "working" && t.state != "paused" {
			t.turnID = rand.Text()
		}
		h.emitSessionStateLocked(t, "error", code)
		return
	}
	h.endTurnLocked(t)
}

func unexpectedSessionExit(status *os.ProcessState) bool {
	if status == nil || status.Success() {
		return false
	}
	wait, ok := status.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT} {
		if wait.Signal() == signal || wait.ExitStatus() == 128+int(signal) {
			return false
		}
	}
	return true
}
