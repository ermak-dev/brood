package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const swarmRPC = "brood-swarm-rpc-v1"

// SwarmRequest contains no sender identity, credentials, address or command.
type SwarmRequest struct {
	Tool        string `json:"tool"`
	To          string `json:"to,omitempty"`
	Message     string `json:"message,omitempty"`
	WaitSeconds int    `json:"wait_seconds,omitempty"`
}

type SwarmResponse struct {
	OK   bool   `json:"ok"`
	Text string `json:"text"`
}

type SwarmCaller func(context.Context, SwarmRequest) SwarmResponse
type SwarmHandler func(context.Context, string, int64, SwarmRequest) SwarmResponse

func validSwarmRequest(r SwarmRequest) bool {
	if r.WaitSeconds < 0 || r.WaitSeconds > 300 {
		return false
	}
	if r.Tool == "agents" {
		return r.To == "" && r.Message == "" && r.WaitSeconds == 0
	}
	return r.Tool == "send" && len(r.To) > 0 && len(r.To) <= 63 && !strings.ContainsAny(r.To, "\x00\r\n") && strings.TrimSpace(r.Message) != "" && len(r.Message) < MaxMessageBytes
}

func DecodeSwarmRequest(data []byte) (SwarmRequest, bool) {
	if len(data) > MaxMessageBytes {
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
		ctx, cancel := context.WithTimeout(p.ctx, time.Duration(input.WaitSeconds)*time.Second+15*time.Second)
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
			var response SwarmResponse
			if err != nil || !ok || len(reply) > MaxMessageBytes || json.Unmarshal(reply, &response) != nil || response.Text == "" {
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
