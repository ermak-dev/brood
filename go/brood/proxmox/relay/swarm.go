package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"

	"brood.dev/go/brood/command"
	"golang.org/x/crypto/ssh"
)

const swarmRPC = "brood-swarm-rpc-v1"

// SwarmRequest is a catalog command of an agent: a tool name and its canonical
// arguments. It contains no sender identity, credentials or address.
type SwarmRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type SwarmResponse struct {
	OK      bool            `json:"ok"`
	Text    string          `json:"text"`
	Data    json.RawMessage `json:"data,omitempty"`
	Outcome string          `json:"outcome,omitempty"`
}

// DecodeSwarmResponse validates the envelope independently of display text.
// Replies without an outcome require nonempty text.
func DecodeSwarmResponse(data []byte) (SwarmResponse, bool) {
	if len(data) > MaxMessageBytes || !uniqueSwarmFields(data) {
		return SwarmResponse{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return SwarmResponse{}, false
	}
	for name := range fields {
		switch name {
		case "ok", "text", "data", "outcome":
		default:
			return SwarmResponse{}, false
		}
	}
	var envelope struct {
		OK      *bool           `json:"ok"`
		Text    *string         `json:"text"`
		Data    json.RawMessage `json:"data"`
		Outcome string          `json:"outcome"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.OK == nil || envelope.Text == nil || bytes.Equal(bytes.TrimSpace(fields["outcome"]), []byte("null")) {
		return SwarmResponse{}, false
	}
	response := SwarmResponse{OK: *envelope.OK, Text: *envelope.Text, Data: envelope.Data, Outcome: envelope.Outcome}
	if response.Outcome == "" {
		return response, response.Text != ""
	}
	if response.OK {
		switch response.Outcome {
		case "ok", "complete", "completed", "accepted", "confirmed", "not_retried", "submitted":
		default:
			return SwarmResponse{}, false
		}
	}
	return response, true
}

type SwarmCaller func(context.Context, SwarmRequest) SwarmResponse
type SwarmHandler func(context.Context, string, int64, SwarmRequest) SwarmResponse

func validSwarmRequest(r SwarmRequest) bool {
	if r.Tool == command.GuestToolList {
		return bytes.Equal(bytes.TrimSpace(r.Arguments), []byte(`{}`))
	}
	spec, ok := command.Lookup(r.Tool)
	if !ok || !spec.Guest {
		return false
	}
	_, err := command.ParseTool(r.Tool, r.Arguments)
	return err == nil
}

func DecodeSwarmRequest(data []byte) (SwarmRequest, bool) {
	if len(data) > MaxMessageBytes || !uniqueSwarmFields(data) {
		return SwarmRequest{}, false
	}
	var r SwarmRequest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
		return r, false
	}
	return r, validSwarmRequest(r)
}

func uniqueSwarmFields(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	open, err := d.Token()
	if err != nil || open != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return false
		}
	}
	_, err = d.Token()
	return err == nil && d.Decode(new(any)) == io.EOF
}

// SwarmCallTimeout bounds one swarm request at the guest broker and at the
// server relay: command.CallTimeout of its wait, so a send without an idle
// wait leaves room for the recipient's delivery receipt (#260).
func SwarmCallTimeout(input SwarmRequest) time.Duration {
	return command.CallTimeout(SwarmWaitSeconds(input))
}

// SwarmWaitSeconds extracts the bounded wait from an already validated request.
func SwarmWaitSeconds(input SwarmRequest) int {
	var args struct {
		WaitSeconds int `json:"wait_seconds"`
	}
	if json.Unmarshal(input.Arguments, &args) != nil {
		return 0
	}
	return args.WaitSeconds
}

func (h *Hub) SetSwarmHandler(handler SwarmHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.swarm = handler
}

func (h *Hub) currentPeer(p *peer) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.closed && h.peers[p.id] == p && h.authorize(p.id.uid, p.id.generation, p.token)
}

func (h *Hub) serveSwarmRequests(p *peer, requests <-chan *ssh.Request) {
	for req := range requests {
		if req.Type != swarmRPC || !req.WantReply {
			_ = req.Reply(false, nil)
			continue
		}
		input, ok := DecodeSwarmRequest(req.Payload)
		h.mu.Lock()
		handler := h.swarm
		h.mu.Unlock()
		if !ok || handler == nil || !h.currentPeer(p) {
			_ = req.Reply(false, nil)
			continue
		}
		ctx, cancel := context.WithTimeout(p.ctx, SwarmCallTimeout(input))
		response := handler(ctx, p.id.uid, p.id.generation, input)
		cancel()
		if !h.currentPeer(p) {
			_ = req.Reply(false, nil)
			continue
		}
		data, err := json.Marshal(response)
		if err != nil || len(data) > MaxMessageBytes {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, data)
	}
}

func swarmCaller(conn ssh.Conn, session context.Context) SwarmCaller {
	slots := make(chan struct{}, MaxMessagesInFlight)
	return func(ctx context.Context, req SwarmRequest) SwarmResponse {
		failed := SwarmResponse{Text: "swarm service unavailable or request refused"}
		if !validSwarmRequest(req) || ctx.Err() != nil || session.Err() != nil {
			return failed
		}
		data, err := json.Marshal(req)
		if err != nil || len(data) > MaxMessageBytes {
			return failed
		}
		select {
		case slots <- struct{}{}:
		default:
			return SwarmResponse{Text: "swarm request limit reached"}
		}
		done := make(chan SwarmResponse, 1)
		go func() {
			defer func() { <-slots }()
			ok, reply, err := conn.SendRequest(swarmRPC, true, data)
			response, valid := DecodeSwarmResponse(reply)
			if err != nil || !ok || !valid {
				done <- failed
				return
			}
			done <- response
		}()
		select {
		case response := <-done:
			return response
		case <-ctx.Done():
			return SwarmResponse{Text: "swarm request timed out; outcome unknown"}
		case <-session.Done():
			return failed
		}
	}
}
