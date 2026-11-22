//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

func sessionHelperSpec() ActorSpec {
	spec := actorSpec("/proc/self/exe")
	spec.Args = []string{"--brood-guest-session"}
	spec.Env = actorEnv()
	return spec
}

// ReadSessionRequest reads only a bounded JSON value, never actor-owned files.
func ReadSessionRequest(r io.Reader) (relay.SessionRequest, error) {
	return relay.ReadSessionRequest(r)
}

// ReceiveSession executes inside the fixed helper after dropping to UID 1000.
func ReceiveSession(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 {
		return relay.SessionResponse{Text: "session helper requires UID 1000", Outcome: "failed"}
	}
	if err := lockPrivileges(); err != nil {
		return relay.SessionResponse{Text: "session helper privilege lock failed", Outcome: "failed"}
	}
	switch req.Operation {
	case "restart":
		return sessionctl.RunWorkspace(ctx, req)
	default:
		return sessionctl.Run(ctx, req)
	}
}

// sessionOutput keeps the helper's response within relay.MaxSessionBytes. The
// buffer is a field, not embedded: os/exec copies the pipe with io.Copy, which
// would take a promoted bytes.Buffer.ReadFrom over the capped Write.
type sessionOutput struct {
	buf    bytes.Buffer
	cancel context.CancelFunc
}

func (b *sessionOutput) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > relay.MaxSessionBytes {
		b.cancel()
		return 0, errors.New("session helper output exceeds size limit")
	}
	return b.buf.Write(p)
}

func (b *sessionOutput) Bytes() []byte { return b.buf.Bytes() }

// NewSessionHandler creates private attachment state for one relay connection.
// Cancellation discards metadata only; neither the PTY host nor Claude is stopped.
func NewSessionHandler(connection context.Context) func(context.Context, relay.SessionRequest) relay.SessionResponse {
	return NewSessionHandlerWithSecrets(connection, nil)
}

// NewSessionHandlerWithSecrets refreshes the root projection under the same
// connection-owned mutation gate as restart, before stopping the session.
// A refresh failure is a known no-session-effect failure, not an unknown delivery.
func NewSessionHandlerWithSecrets(connection context.Context, refresh func(context.Context) error) func(context.Context, relay.SessionRequest) relay.SessionResponse {
	return newSessionHandlerWithSecrets(connection, refresh, HandleSession)
}

func newSessionHandlerWithSecrets(connection context.Context, refresh func(context.Context) error, run func(context.Context, relay.SessionRequest) relay.SessionResponse) func(context.Context, relay.SessionRequest) relay.SessionResponse {
	controller := NewTerminalController(func(ctx context.Context, step sessionctl.TerminalStep) relay.SessionResponse {
		data, err := json.Marshal(step)
		if err != nil {
			return terminalFailure("invalid terminal step")
		}
		return runSessionHelper(ctx, relay.SessionRequest{Operation: "session_terminal", Arguments: data, Caller: "supervisor"})
	})
	context.AfterFunc(connection, controller.Close)
	return func(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
		if connection.Err() != nil {
			controller.Close()
			return terminalFailure("terminal connection is closed")
		}
		if req.Operation == "session_terminal" {
			return controller.Handle(ctx, req)
		}
		switch req.Operation {
		case "slash", "restart":
			return controller.Control(ctx, func() relay.SessionResponse {
				if req.Operation == "restart" && !refreshSessionSecrets(ctx, connection, refresh) {
					return terminalFailure("project secrets refresh failed; session unchanged")
				}
				return run(ctx, req)
			})
		default:
			return run(ctx, req)
		}
	}
}

// refreshSessionSecrets stages a projection only while both the request and
// the connection still authorize the session replacement.
func refreshSessionSecrets(ctx, authority context.Context, refresh func(context.Context) error) bool {
	if refresh == nil {
		return true
	}
	bound, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(authority, cancel)
	defer cancel()
	defer stop()
	return authority.Err() == nil && bound.Err() == nil && refresh(bound) == nil &&
		bound.Err() == nil && authority.Err() == nil
}

// HandleSession handles stateless operations. Terminal requests require the
// connection-owned NewSessionHandler and cannot bypass its attachment checks.
func HandleSession(parent context.Context, req relay.SessionRequest) relay.SessionResponse {
	if req.Operation == "session_terminal" {
		return terminalFailure("terminal requires a connection-owned session handler")
	}
	return runSessionHelper(parent, req)
}

// runSessionHelper fixes the executable, argv, environment and credentials;
// only bounded JSON crosses the UID fence.
func runSessionHelper(parent context.Context, req relay.SessionRequest) relay.SessionResponse {
	fail := func(text string) relay.SessionResponse { return relay.SessionResponse{Text: text, Outcome: "failed"} }
	if os.Geteuid() != 0 {
		return fail("session operations require guest supervisor")
	}
	ctx, cancel := context.WithTimeout(parent, 18*time.Second)
	defer cancel()
	data, err := json.Marshal(req)
	if err != nil || len(data) > relay.MaxSessionBytes || ctx.Err() != nil {
		return fail("invalid session request")
	}
	if _, err := ReadSessionRequest(bytes.NewReader(data)); err != nil {
		return fail("invalid session request")
	}
	cmd, err := guestHelperCommand(sessionHelperSpec())
	if err != nil {
		return fail("session helper unavailable")
	}
	output := &sessionOutput{cancel: cancel}
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return fail("session helper unavailable")
	}
	uncertain := req.Operation == "slash" || req.Operation == "restart"
	if req.Operation == "session_terminal" {
		var step sessionctl.TerminalStep
		_ = json.Unmarshal(req.Arguments, &step)
		uncertain = step.Action == "input" || step.Action == "interrupt" || step.Action == "resize"
	}
	stopped := func(text string) relay.SessionResponse {
		if uncertain {
			return relay.SessionResponse{Text: text + "; delivery is unknown, do not retry automatically", Outcome: "unknown"}
		}
		return fail(text)
	}
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	defer stop()
	if err := cmd.Wait(); err != nil {
		return stopped("session helper stopped or exceeded its resource limit")
	}
	var response relay.SessionResponse
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&response) != nil || decoder.Decode(new(any)) != io.EOF || response.Outcome == "" {
		return stopped("invalid session helper response")
	}
	return response
}
