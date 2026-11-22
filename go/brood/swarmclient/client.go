// Package swarmclient implements the credential-free guest broker transport.
package swarmclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"brood.dev/go/brood/command"
)

const socketPath = "/run/brood/swarm.sock"
const maxFrameBytes = 128 * 1024

// Client always uses the fixed guest socket. A broker failure never authorizes
// operator fallback or selecting another identity or endpoint.
type Client struct{}

func (c Client) ListGuestTools(ctx context.Context) ([]string, error) {
	result, err := c.Execute(ctx, command.Invocation{Name: command.GuestToolList})
	if err != nil {
		return nil, err
	}
	var tools []string
	err = json.Unmarshal(result.Data, &tools)
	return tools, err
}

// Execute sends one validated guest command to the local supervisor broker.
func (Client) Execute(ctx context.Context, invocation command.Invocation) (command.Result, error) {
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return command.Result{}, errors.New("swarm broker unavailable")
	}
	defer conn.Close()
	return exchange(ctx, conn, invocation)
}

// Request is the broker wire envelope: a tool name and its canonical arguments.
type Request struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// NewRequest validates a catalog invocation and builds the scoped broker wire.
func NewRequest(invocation command.Invocation) (Request, error) {
	if invocation.Name == command.GuestToolList {
		return Request{Tool: invocation.Name, Arguments: json.RawMessage(`{}`)}, nil
	}
	invocation, err := command.ParseTool(invocation.Name, invocation.Arguments)
	if err != nil {
		return Request{}, err
	}
	spec, _ := command.Lookup(invocation.Name)
	if !spec.Guest {
		return Request{}, errors.New("command unavailable in guest mode")
	}
	return Request{Tool: invocation.Name, Arguments: invocation.Arguments}, nil
}

func requestTimeout(request Request) time.Duration {
	var args struct {
		WaitSeconds int `json:"wait_seconds"`
	}
	_ = json.Unmarshal(request.Arguments, &args)
	if args.WaitSeconds < 0 || args.WaitSeconds > 300 {
		args.WaitSeconds = 0
	}
	return command.CallTimeout(args.WaitSeconds)
}

type responseEnvelope struct {
	OK      bool            `json:"ok"`
	Text    string          `json:"text"`
	Data    json.RawMessage `json:"data,omitempty"`
	Outcome string          `json:"outcome,omitempty"`
}

// decodeResponse keeps the guest client independent of the SSH relay package.
func decodeResponse(data []byte) (responseEnvelope, bool) {
	if len(data) > maxFrameBytes {
		return responseEnvelope{}, false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	open, err := d.Token()
	if err != nil || open != json.Delim('{') {
		return responseEnvelope{}, false
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || fields[name] != nil {
			return responseEnvelope{}, false
		}
		switch name {
		case "ok", "text", "data", "outcome":
		default:
			return responseEnvelope{}, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return responseEnvelope{}, false
		}
		fields[name] = value
	}
	if _, err = d.Token(); err != nil || d.Decode(new(any)) != io.EOF {
		return responseEnvelope{}, false
	}
	var envelope struct {
		OK      *bool           `json:"ok"`
		Text    *string         `json:"text"`
		Data    json.RawMessage `json:"data"`
		Outcome string          `json:"outcome"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.OK == nil || envelope.Text == nil || bytes.Equal(bytes.TrimSpace(fields["outcome"]), []byte("null")) {
		return responseEnvelope{}, false
	}
	response := responseEnvelope{OK: *envelope.OK, Text: *envelope.Text, Data: envelope.Data, Outcome: envelope.Outcome}
	if response.Outcome == "" {
		return response, response.Text != ""
	}
	if response.OK {
		switch response.Outcome {
		case "ok", "complete", "completed", "accepted", "confirmed", "not_retried", "submitted":
		default:
			return responseEnvelope{}, false
		}
	}
	return response, true
}

func exchange(ctx context.Context, conn net.Conn, invocation command.Invocation) (command.Result, error) {
	failed := func() (command.Result, error) {
		return command.Result{Outcome: "unconfirmed"}, errors.New("swarm broker unavailable; outcome unconfirmed")
	}
	request, err := NewRequest(invocation)
	if err != nil {
		return command.Result{}, err
	}
	wire, err := json.Marshal(request)
	if err != nil || len(wire)+1 > maxFrameBytes {
		return command.Result{}, errors.New("swarm request exceeds frame limit")
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(requestTimeout(request))
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	_ = conn.SetDeadline(deadline)
	if _, err = io.Copy(conn, bytes.NewReader(append(wire, '\n'))); err != nil {
		return failed()
	}
	data, err := bufio.NewReader(io.LimitReader(conn, maxFrameBytes+1)).ReadBytes('\n')
	response, valid := decodeResponse(data)
	if err != nil || !valid {
		return failed()
	}
	result := command.Result{Text: response.Text, Data: response.Data, Outcome: response.Outcome}
	if !response.OK {
		return result, errors.New(response.Text)
	}
	return result, nil
}
