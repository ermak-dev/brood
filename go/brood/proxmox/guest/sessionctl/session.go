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
	"strings"
	"sync"
	"time"
	"unicode"

	"brood.dev/go/brood/proxmox/guest/ptysession"
	"brood.dev/go/brood/proxmox/relay"
)

const fixedConfig = "/workspace/home/.claude"

type identity struct {
	SessionID   string
	Process     string
	PID         int
	Socket      string
	Device      uint64
	Inode       uint64
	HolderProof string
	Epoch       string
}
type engine struct {
	config string
	prove  func(context.Context) (identity, error)
	call   ptyCall
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
	e := engine{config: fixedConfig, prove: proveLive, call: callPTY, now: time.Now}
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
	if req.Operation == relay.StatusOperation {
		return e.status(ctx, req.Arguments)
	}
	if req.Operation != "tail" && req.Operation != "slash" {
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
		if err != nil || before.HolderProof == "" {
			return failure("no proven live Claude session and PTY holder")
		}
		reply, err := e.call(ctx, ptysession.Request{Action: "tail", Proof: before.HolderProof, Lines: a.Lines})
		if err != nil || !reply.OK {
			return ptyFailure(reply, err, false)
		}
		text := reply.Screen
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
		// Команда начинается с "/" и умещается в строку ввода; Escape —
		// sentinel прерывания текущего хода, не команда.
		if a.Command == "" || (a.Command != "Escape" && !strings.HasPrefix(a.Command, "/")) {
			return failure("slash command must start with / or be Escape")
		}
		if len(a.Command) > 255 {
			return failure("slash command is too long")
		}
		before, err := e.prove(ctx)
		if err != nil || before.HolderProof == "" {
			return failure("no proven live Claude session and PTY holder")
		}
		var baseline string
		var capture bool
		if strings.HasPrefix(req.Caller, "telegram:") && a.Command != "Escape" {
			baseline, capture = e.slashSnapshot(ctx, before.HolderProof)
		}
		data := []byte(a.Command + "\r")
		if a.Command == "Escape" {
			data = []byte{0x1b}
		}
		reply, err := e.call(ctx, ptysession.Request{Action: "input", Proof: before.HolderProof, Data: data})
		if err != nil || !reply.OK {
			return ptyFailure(reply, err, true)
		}
		after, err := e.prove(ctx)
		if err != nil || before != after {
			return relay.SessionResponse{Text: "session changed after input; delivery is unknown", Outcome: "unknown"}
		}
		if capture {
			if output := e.waitSlashOutput(ctx, before.HolderProof, a.Command, baseline); output != "" {
				return relay.SessionResponse{OK: true, Text: output, Outcome: "observed"}
			}
		}
		return relay.SessionResponse{OK: true, Text: "input submitted; command completion is not confirmed", Outcome: "submitted"}
	}
	return failure("unsupported operation")
}

// cappedOutput keeps at most 48 KiB of process output and cancels the process
// past it. The buffer is a field, not embedded: os/exec copies the pipe with
// io.Copy, which would take a promoted bytes.Buffer.ReadFrom over Write.
type cappedOutput struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	cancel context.CancelFunc
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len()+len(p) > 48*1024 {
		b.cancel()
		return 0, errors.New("process output exceeds limit")
	}
	return b.buf.Write(p)
}

func (b *cappedOutput) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *cappedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
