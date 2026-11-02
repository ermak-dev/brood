//go:build linux

package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

// TerminalResponse explicitly describes snapshot semantics. Cursor identifies a
// whole visible screen, not a byte offset or a claim of lossless PTY history.
type TerminalResponse struct {
	TID       string `json:"tid"`
	Mode      string `json:"mode"`
	Seq       uint64 `json:"seq"`
	Semantics string `json:"semantics"`
	Epoch     string `json:"epoch"`
	Cursor    string `json:"cursor,omitempty"`
	Reset     bool   `json:"reset"`
	Output    string `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Closed    bool   `json:"closed,omitempty"`
}

type terminalArguments struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	TID    string `json:"tid"`
	Mode   string `json:"mode"`
	Seq    uint64 `json:"seq"`
	Data   string `json:"data"`
	Cols   *int   `json:"cols"`
	Rows   *int   `json:"rows"`
	Cursor string `json:"cursor"`
	Wait   int    `json:"wait"`
}

type terminalAttachment struct {
	caller, mode, proof, epoch string
	last                       time.Time
	seq                        uint64
	hash                       [32]byte
	cached                     relay.SessionResponse
	closed, disabled, invalid  bool
}

// TerminalController owns only bounded metadata. Its helper must execute inside
// the fixed UID 1000 process; no actor-owned file is opened by this controller.
type TerminalController struct {
	mu          sync.Mutex
	attachments map[string]*terminalAttachment
	step        func(context.Context, sessionctl.TerminalStep) relay.SessionResponse
	now         func() time.Time
	closed      bool
}

// NewTerminalController creates isolated state for one relay connection.
func NewTerminalController(step func(context.Context, sessionctl.TerminalStep) relay.SessionResponse) *TerminalController {
	return &TerminalController{attachments: make(map[string]*terminalAttachment), step: step, now: time.Now}
}

// Close drops all attachment metadata on disconnect without touching tmux.
func (c *TerminalController) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	clear(c.attachments)
}
func terminalFailure(text string) relay.SessionResponse {
	return relay.SessionResponse{Outcome: "failed", Text: text}
}
func terminalResponse(value TerminalResponse) relay.SessionResponse {
	b, _ := json.Marshal(value)
	return relay.SessionResponse{OK: true, Outcome: "complete", Text: string(b)}
}
func randomTerminalID() (string, error) {
	var b [24]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func (c *TerminalController) prune(now time.Time) {
	for tid, a := range c.attachments {
		ttl := 60 * time.Second
		if a.mode == "rw" {
			ttl = 30 * time.Second
		}
		if now.Sub(a.last) >= ttl {
			delete(c.attachments, tid)
		}
	}
}

func parseTerminalArguments(raw []byte) (terminalArguments, error) {
	var a terminalArguments
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return a, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return a, io.ErrUnexpectedEOF
	}
	// Null fields are not defaults: explicit null and non-objects fail closed.
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return a, io.ErrUnexpectedEOF
	}
	for _, v := range fields {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return a, io.ErrUnexpectedEOF
		}
	}
	return a, nil
}

// Handle processes the public session_terminal API, never forwarding caller
// arguments as internal helper steps. Every mutation consumes sequence first.
func (c *TerminalController) Handle(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	if len(req.Arguments) > 16*1024 || req.Caller == "" || len(req.Caller) > 512 || req.Operation != "session_terminal" {
		return terminalFailure("invalid terminal request")
	}
	args, err := parseTerminalArguments(req.Arguments)
	if err != nil || args.Wait < 0 || args.Wait > 15 || (args.Action != "read" && args.Wait != 0) || len(args.Name) > 512 || len(args.TID) > 128 || len(args.Cursor) > 256 || len(args.Data) > 4096 || !utf8.ValidString(args.Data) {
		return terminalFailure("invalid terminal arguments")
	}
	cols, rows := 80, 24
	if args.Cols != nil {
		cols = *args.Cols
	}
	if args.Rows != nil {
		rows = *args.Rows
	}
	if cols < 20 || cols > 240 || rows < 5 || rows > 80 {
		return terminalFailure("terminal dimensions must be cols 20..240 and rows 5..80")
	}
	switch args.Action {
	case "open", "read", "input", "interrupt", "resize", "close":
	default:
		return terminalFailure("unsupported terminal action")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || ctx.Err() != nil {
		return terminalFailure("terminal connection is closed")
	}
	now := c.now()
	c.prune(now)
	if args.Action == "open" {
		if args.TID != "" || args.Seq != 0 || args.Data != "" || args.Cursor != "" {
			return terminalFailure("open must not supply attachment state")
		}
		mode := args.Mode
		if mode == "" {
			mode = "ro"
		}
		if mode != "ro" && mode != "rw" {
			return terminalFailure("terminal mode must be ro or rw")
		}
		if len(c.attachments) >= 4 {
			return terminalFailure("terminal attachment limit reached")
		}
		tid, err := randomTerminalID()
		if err != nil {
			return terminalFailure("terminal ID unavailable")
		}
		r := c.step(ctx, sessionctl.TerminalStep{Action: "open"})
		if !r.OK {
			return r
		}
		var result sessionctl.TerminalResult
		if json.Unmarshal([]byte(r.Text), &result) != nil || result.Proof == "" || len(result.Proof) > 128 {
			return terminalFailure("invalid terminal helper proof")
		}
		for _, a := range c.attachments {
			if mode == "rw" && a.mode == "rw" && !a.closed && a.proof == result.Proof {
				return terminalFailure("a terminal writer already owns this session")
			}
		}
		a := &terminalAttachment{caller: req.Caller, mode: mode, proof: result.Proof, epoch: tid, last: now}
		c.attachments[tid] = a
		if mode == "rw" {
			r = c.step(ctx, sessionctl.TerminalStep{Action: "resize", Proof: a.proof, Cols: cols, Rows: rows})
			if !r.OK {
				a.disabled = true
				return r
			}
		}
		return terminalResponse(TerminalResponse{TID: tid, Mode: mode, Semantics: "screen-snapshot", Epoch: a.epoch})
	}
	a := c.attachments[args.TID]
	if a == nil || a.caller != req.Caller {
		return terminalFailure("terminal attachment not found for caller")
	}
	// Hash the exact bounded request; ambiguous same-sequence payloads are refused.
	hash := sha256.Sum256(req.Arguments)
	if args.Seq == a.seq && a.seq != 0 {
		if hash == a.hash {
			return a.cached
		}
		return terminalFailure("terminal sequence was used with a different request")
	}
	if a.closed || (a.invalid && args.Action != "close") {
		return terminalFailure("terminal attachment is closed or stale")
	}
	if args.Seq == 0 || a.seq == ^uint64(0) || args.Seq != a.seq+1 {
		return terminalFailure("terminal sequence must be consecutive starting at 1")
	}
	mutation := args.Action == "input" || args.Action == "interrupt" || args.Action == "resize"
	if mutation && (a.mode != "rw" || a.disabled) {
		return terminalFailure("terminal attachment is read-only or writer disabled")
	}
	if args.Action == "input" && args.Data == "" {
		return terminalFailure("terminal input must not be empty")
	}
	defaultDimensions := (args.Cols == nil || *args.Cols == 100) && (args.Rows == nil || *args.Rows == 30)
	if (args.Mode != "" && args.Mode != "ro") || (args.Action != "input" && args.Data != "") || (args.Action != "read" && args.Cursor != "") || (args.Action != "resize" && !defaultDimensions) {
		return terminalFailure("arguments do not match terminal action")
	}
	a.seq = args.Seq
	a.hash = hash
	a.last = now
	a.cached = relay.SessionResponse{Outcome: "unknown", Text: "terminal operation did not complete; do not retry automatically"}
	response := TerminalResponse{TID: args.TID, Mode: a.mode, Seq: a.seq, Semantics: "screen-snapshot", Epoch: a.epoch}
	if args.Action == "close" {
		a.closed = true
		response.Closed = true
		a.cached = terminalResponse(response)
		return a.cached
	}
	r := c.step(ctx, sessionctl.TerminalStep{Action: args.Action, Proof: a.proof, Data: args.Data, Cols: cols, Rows: rows})
	if !r.OK {
		if mutation {
			a.disabled = true
		}
		if r.Outcome != "unknown" {
			a.invalid = true
		}
		a.cached = r
		return r
	}
	var result sessionctl.TerminalResult
	if json.Unmarshal([]byte(r.Text), &result) != nil || result.Proof != a.proof || len(result.Output) > 16*1024 {
		a.invalid = true
		a.disabled = true
		outcome := "failed"
		if mutation {
			outcome = "unknown"
		}
		a.cached = relay.SessionResponse{Outcome: outcome, Text: "terminal helper proof or output is invalid"}
		return a.cached
	}
	if args.Action == "read" {
		screen, _ := json.Marshal(struct {
			Output    string
			Truncated bool
		}{result.Output, result.Truncated})
		sum := sha256.Sum256(screen)
		cursor := a.epoch + ":" + hex.EncodeToString(sum[:])
		if args.Wait > 0 && args.Cursor == cursor {
			deadline := time.NewTimer(time.Duration(args.Wait) * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
		waitSnapshot:
			for {
				select {
				case <-ctx.Done():
					a.cached = terminalFailure("terminal read canceled")
					return a.cached
				case <-deadline.C:
					break waitSnapshot
				case <-ticker.C:
					r := c.step(ctx, sessionctl.TerminalStep{Action: "read", Proof: a.proof})
					if !r.OK {
						a.invalid = true
						a.cached = r
						return r
					}
					result = sessionctl.TerminalResult{}
					if json.Unmarshal([]byte(r.Text), &result) != nil || result.Proof != a.proof || len(result.Output) > 16*1024 {
						a.invalid = true
						a.cached = terminalFailure("terminal session changed during read")
						return a.cached
					}
					screen, _ = json.Marshal(struct {
						Output    string
						Truncated bool
					}{result.Output, result.Truncated})
					sum = sha256.Sum256(screen)
					cursor = a.epoch + ":" + hex.EncodeToString(sum[:])
					if cursor != args.Cursor {
						break waitSnapshot
					}
				}
			}
		}
		response.Reset = args.Cursor != cursor
		if response.Reset {
			response.Output = result.Output
			response.Truncated = result.Truncated
		}
		response.Cursor = cursor
	}
	a.cached = terminalResponse(response)
	return a.cached
}
