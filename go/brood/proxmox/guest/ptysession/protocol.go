//go:build linux

// Package ptysession owns a persistent Linux PTY and its local attachment protocol.
package ptysession

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const SocketPath = "/run/brood/pty.sock"

// MaxMessage bounds encoded protocol records, including replay snapshots.
const MaxMessage = 2 * 1024 * 1024

// Config fixes the trusted child command, environment, directory and initial size.
type Config struct {
	Command        []string
	BeforeStart    func() ([]string, error) // Returns extra environment for each new child.
	OnSessionEvent func(SessionEvent)       // Enqueues an event without blocking the PTY host.
	Env            []string
	Dir            string
	Columns, Rows  int
}

// State identifies one owned child instance.
type State struct {
	PID     int    `json:"pid"`
	Start   string `json:"start"`
	Epoch   string `json:"epoch"`
	Running bool   `json:"running"`
	Columns int    `json:"columns"`
	Rows    int    `json:"rows"`
}

// Request starts every connection. Attach switches to Frame after its Reply.
type Request struct {
	Action   string       `json:"action"`
	Proof    string       `json:"proof,omitempty"`
	Data     []byte       `json:"data,omitempty"`
	Columns  int          `json:"columns,omitempty"`
	Rows     int          `json:"rows,omitempty"`
	Lines    int          `json:"lines,omitempty"`
	ReadOnly bool         `json:"readOnly,omitempty"`
	Hook     *SessionHook `json:"hook,omitempty"`
}

// Reply reports complete, refused, or unknown mutation outcome.
type Reply struct {
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
	State   State  `json:"state"`
	Proof   string `json:"proof,omitempty"`
	Screen  string `json:"screen,omitempty"`
	Replay  []byte `json:"replay,omitempty"`
	// Turn and TurnError answer only the status action: the state of the
	// current turn (working, paused, idle or error) and the code of its failure.
	Turn      string `json:"turn,omitempty"`
	TurnError string `json:"turnError,omitempty"`
}

// Frame carries unchanged output or writer-only input and resize operations.
type Frame struct {
	Type    string `json:"type"`
	Data    []byte `json:"data,omitempty"`
	Columns int    `json:"columns,omitempty"`
	Rows    int    `json:"rows,omitempty"`
}

// ReadMessage reads one bounded newline-delimited record and rejects unknown fields.
// Retain the reader for the lifetime of a connection: it may buffer following frames.
func ReadMessage(r *bufio.Reader, value any) error {
	var record []byte
	for {
		fragment, err := r.ReadSlice('\n')
		if len(record)+len(fragment) > MaxMessage {
			return errors.New("PTY message exceeds limit")
		}
		record = append(record, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	dec := json.NewDecoder(bytes.NewReader(record))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return fmt.Errorf("invalid PTY message: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("PTY message must contain one JSON value")
	}
	return nil
}

// WriteMessage writes one bounded record. The caller serializes concurrent writes.
func WriteMessage(w io.Writer, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b)+1 > MaxMessage {
		return errors.New("PTY message exceeds limit")
	}
	b = append(b, '\n')
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// Call performs one RPC. Attach users retain their own connection instead.
func Call(ctx context.Context, path string, request Request) (Reply, error) {
	var reply Reply
	if request.Action == "attach" {
		return reply, errors.New("attach requires a persistent connection")
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return reply, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)
	if err = WriteMessage(c, request); err != nil {
		return reply, err
	}
	err = ReadMessage(bufio.NewReader(c), &reply)
	return reply, err
}
