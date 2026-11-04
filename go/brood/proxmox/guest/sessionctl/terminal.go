//go:build linux

package sessionctl

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"brood.dev/go/brood/proxmox/guest/ptysession"
	"brood.dev/go/brood/proxmox/relay"
)

// TerminalStep is an internal supervisor-to-helper command, not a public request.
// Proof is opaque metadata; the supervisor never interprets or follows a path.
type TerminalStep struct {
	Action string `json:"action"`
	Proof  string `json:"proof,omitempty"`
	Data   string `json:"data,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
}

// TerminalResult contains a bounded visible-screen snapshot, never PTY history.
type TerminalResult struct {
	Proof     string `json:"proof"`
	Output    string `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func terminalProof(id identity) string { b, _ := json.Marshal(id); return digest(b) }

func (e *engine) terminal(ctx context.Context, raw []byte) relay.SessionResponse {
	var s TerminalStep
	if decodeArgs(raw, &s) != nil || len(s.Proof) > 128 || len(s.Data) > 4096 || !utf8.ValidString(s.Data) {
		return failure("invalid internal terminal step")
	}
	switch s.Action {
	case "open", "read", "input", "interrupt":
	case "resize":
		if s.Cols < 20 || s.Cols > 240 || s.Rows < 5 || s.Rows > 80 {
			return failure("terminal dimensions must be cols 20..240 and rows 5..80")
		}
	default:
		return failure("unsupported internal terminal action")
	}
	if s.Action != "open" && s.Proof == "" {
		return failure("terminal session proof is required")
	}
	before, err := e.prove(ctx)
	if err != nil || before.HolderProof == "" || (s.Proof != "" && terminalProof(before) != s.Proof) {
		return failure("terminal session identity is no longer live")
	}
	result := TerminalResult{Proof: terminalProof(before)}
	if s.Action == "open" {
		return success(result)
	}
	req := ptysession.Request{Proof: before.HolderProof}
	mutation := s.Action != "read"
	switch s.Action {
	case "read":
		req.Action = "screen"
	case "input":
		if s.Data == "" {
			return failure("terminal input must not be empty")
		}
		req.Action, req.Data = "input", []byte(s.Data)
	case "interrupt":
		req.Action, req.Data = "input", []byte{0x1b}
	case "resize":
		req.Action, req.Columns, req.Rows = "resize", s.Cols, s.Rows
	}
	reply, err := e.call(ctx, req)
	if err != nil || !reply.OK {
		return ptyFailure(reply, err, mutation)
	}
	text := reply.Screen
	after, err := e.prove(ctx)
	if err != nil || before != after {
		if mutation {
			return relay.SessionResponse{Outcome: "unknown", Text: "session changed after terminal mutation; writer disabled"}
		}
		return failure("session changed during terminal snapshot")
	}
	if !mutation {
		if len(text) > 16*1024 {
			text = text[:16*1024]
			for !utf8.ValidString(text) && len(text) > 0 {
				text = text[:len(text)-1]
			}
			result.Truncated = true
		}
		if !utf8.ValidString(text) {
			return failure("terminal snapshot is not UTF-8")
		}
		result.Output = text
	}
	return success(result)
}
