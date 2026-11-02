//go:build linux

// Package sessionctl implements fixed, unprivileged guest session operations.
package sessionctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"brood.dev/go/brood/proxmox/relay"
)

const fixedConfig = "/workspace/home/.claude"
const target = "claude:0.0"
const pageBudget = 24 * 1024

type identity struct {
	SessionID string
	Process   string
	PID       int
	Socket    string
	Device    uint64
	Inode     uint64
}
type engine struct {
	config string
	prove  func(context.Context) (identity, error)
	run    func(context.Context, ...string) (string, error)
	now    func() time.Time
}

func failure(text string) relay.SessionResponse {
	return relay.SessionResponse{Text: text, Outcome: "failed"}
}
func success(value any) relay.SessionResponse {
	data, err := json.Marshal(value)
	if err != nil {
		return failure("session response encoding failed")
	}
	resp := relay.SessionResponse{OK: true, Text: string(data), Outcome: "complete"}
	wire, _ := json.Marshal(resp)
	if len(wire)+1 > relay.MaxSessionBytes {
		return failure("session response exceeds size limit")
	}
	return resp
}

// Run executes only after the fixed helper has dropped all privileges.
func Run(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return failure("session helper requires UID and GID 1000")
	}
	groups, err := os.Getgroups()
	if err != nil {
		return failure("session helper group check failed")
	}
	for _, g := range groups {
		if g != 1000 {
			return failure("session helper has unexpected supplementary groups")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	e := engine{config: fixedConfig, prove: proveLive, run: runTMUX, now: time.Now}
	return e.handle(ctx, req)
}

func decodeArgs(raw []byte, dst any) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("arguments must be an object")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return errors.New("arguments must be an object")
	}
	for _, v := range fields {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return errors.New("null argument")
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid operation arguments")
	}
	return nil
}
func control(s string) bool { return strings.IndexFunc(s, unicode.IsControl) >= 0 }

func (e *engine) handle(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	if req.Operation == "session_terminal" {
		return e.terminal(ctx, req.Arguments)
	}
	if req.Operation == "wipe" || req.Operation == "recycle" || req.Operation == "workspace" {
		return relay.SessionResponse{Text: "operation is not implemented safely; no session or workspace was changed", Outcome: "unsupported"}
	}
	if req.Operation != "tail" && req.Operation != "slash" && req.Operation != "session_history" && req.Operation != "stat" {
		return relay.SessionResponse{Text: "unsupported session operation", Outcome: "unsupported"}
	}
	if err := ctx.Err(); err != nil {
		return failure("session operation canceled")
	}
	// name — только адрес сервера; локальный helper никогда не выбирает им путь.
	switch req.Operation {
	case "tail":
		a := struct {
			Name  string `json:"name"`
			Lines int    `json:"lines"`
			Grep  string `json:"grep"`
		}{Lines: 40}
		if decodeArgs(req.Arguments, &a) != nil || a.Lines < 1 || a.Lines > 10000 || len(a.Grep) > 1024 || control(a.Grep) {
			return failure("invalid tail arguments")
		}
		before, err := e.prove(ctx)
		if err != nil {
			return failure("no proven live Claude session")
		}
		text, err := e.run(ctx, "capture-pane", "-p", "-t", target, "-S", "-"+strconv.Itoa(a.Lines))
		if err != nil {
			return failure("tmux screen unavailable or output exceeds size limit")
		}
		after, err := e.prove(ctx)
		if err != nil || before != after {
			return failure("live session changed during screen read")
		}
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		if len(lines) > a.Lines {
			text = strings.Join(lines[len(lines)-a.Lines:], "\n") + "\n"
		}
		if a.Grep != "" {
			var lines []string
			for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
				if strings.Contains(line, a.Grep) {
					lines = append(lines, line)
				}
			}
			text = strings.Join(lines, "\n")
			if len(lines) > 0 {
				text += "\n"
			}
		}
		resp := relay.SessionResponse{OK: true, Text: text, Outcome: "complete"}
		data, _ := json.Marshal(resp)
		if len(data)+1 > relay.MaxSessionBytes {
			return failure("screen exceeds response limit")
		}
		return resp
	case "slash":
		var a struct {
			Name    string `json:"name"`
			Command string `json:"command"`
		}
		if decodeArgs(req.Arguments, &a) != nil || control(a.Command) {
			return failure("invalid slash arguments")
		}
		allowed := false
		for _, s := range []string{"/model", "/clear", "/compact", "/rc", "/status", "Escape"} {
			if a.Command == s {
				allowed = true
			}
		}
		if !allowed {
			return failure("slash command is not allowlisted")
		}
		before, err := e.prove(ctx)
		if err != nil {
			return failure("no proven live Claude session")
		}
		args := []string{"send-keys", "-t", target, "-l", "--", a.Command}
		if a.Command == "Escape" {
			args = []string{"send-keys", "-t", target, "Escape"}
		}
		if _, err = e.run(ctx, args...); err != nil {
			return relay.SessionResponse{Text: "input delivery is unknown; do not retry automatically", Outcome: "unknown"}
		}
		after, err := e.prove(ctx)
		if err != nil || before != after {
			return relay.SessionResponse{Text: "session changed after input; delivery is unknown", Outcome: "unknown"}
		}
		if a.Command != "Escape" {
			if _, err = e.run(ctx, "send-keys", "-t", target, "Enter"); err != nil {
				return relay.SessionResponse{Text: "command submission is unknown; do not retry automatically", Outcome: "unknown"}
			}
		}
		return relay.SessionResponse{OK: true, Text: "input submitted; command completion is not confirmed", Outcome: "submitted"}
	case "session_history":
		a := struct {
			Name   string `json:"name"`
			Cursor string `json:"cursor"`
			Limit  int    `json:"limit"`
		}{Limit: 100}
		if decodeArgs(req.Arguments, &a) != nil || a.Limit < 1 || a.Limit > 1000 || len(a.Cursor) > 4096 {
			return failure("invalid history arguments")
		}
		return e.history(ctx, a.Cursor, a.Limit)
	case "stat":
		a := struct {
			Name string `json:"name"`
			Days int    `json:"days"`
		}{Days: 14}
		if decodeArgs(req.Arguments, &a) != nil || a.Days < 1 || a.Days > 366 {
			return failure("days must be between 1 and 366")
		}
		return e.usage(ctx, a.Days)
	}
	return failure("unsupported operation")
}

type cappedOutput struct {
	mu sync.Mutex
	bytes.Buffer
	cancel context.CancelFunc
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Len()+len(p) > 48*1024 {
		b.cancel()
		return 0, errors.New("process output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func runTMUX(parent context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/tmux", args...)
	cmd.Env = []string{"HOME=/workspace/home", "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.Dir = "/workspace/project"
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
	return output.String(), err
}
