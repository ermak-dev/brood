//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"brood.dev/go/brood/proxmox/guest/ccmsg"
	"brood.dev/go/brood/proxmox/relay"
)

func messageHelperSpec() ActorSpec {
	s := actorSpec("/proc/self/exe")
	s.Args = []string{"--brood-guest-message"}
	s.Env = []string{"HOME=/workspace/home", "PWD=/workspace/project", "USER=claude", "LOGNAME=claude", "PATH=/usr/local/bin:/usr/bin:/bin", "GOMAXPROCS=4"}
	return s
}

// ReadDeliveryRequest parses one bounded helper request with no socket paths,
// executable names or additional JSON values. It does not open actor files.
func ReadDeliveryRequest(r io.Reader) (relay.MessageRequest, error) {
	data, err := io.ReadAll(io.LimitReader(r, relay.MaxMessageBytes+1))
	if err != nil || len(data) > relay.MaxMessageBytes {
		return relay.MessageRequest{}, errors.New("invalid message helper input")
	}
	var req relay.MessageRequest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		return relay.MessageRequest{}, errors.New("invalid message helper input")
	}
	if err := d.Decode(new(any)); err != io.EOF || req.To == "" || req.From == "" || req.Body == "" || req.WaitSec < 0 || req.WaitSec > 300 {
		return relay.MessageRequest{}, errors.New("invalid message helper input")
	}
	return req, nil
}

// ReceiveMessage runs only inside the fixed, unprivileged delivery helper.
// Paths from actor-owned session files are never opened by the root supervisor.
func ReceiveMessage(ctx context.Context, req relay.MessageRequest) (relay.MessageResponse, error) {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return relay.MessageResponse{}, errors.New("message helper requires UID and GID 1000")
	}
	groups, err := os.Getgroups()
	if err != nil {
		return relay.MessageResponse{}, errors.New("message helper group check failed")
	}
	for _, g := range groups {
		if g != 1000 {
			return relay.MessageResponse{}, errors.New("message helper has unexpected supplementary groups")
		}
	}
	if err := lockPrivileges(); err != nil {
		return relay.MessageResponse{}, err
	}
	const sessions = "/workspace/home/.claude/sessions"
	session, err := ccmsg.SessionForCWD(sessions, "/workspace/project", func(path string) bool {
		probe, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
		if err != nil {
			return false
		}
		probe.Close()
		return true
	})
	if err != nil {
		return relay.MessageResponse{}, errors.New("no unique live Claude session for this project")
	}
	token, err := ccmsg.PeerToken(session.SocketPath, sessions)
	if err != nil {
		return relay.MessageResponse{}, errors.New("Claude session peer key unavailable")
	}
	result, err := ccmsg.Exchange(ctx, session.SocketPath, token, req.From, req.Body, req.WaitSec)
	if err != nil {
		return relay.MessageResponse{}, errors.New("Claude session message exchange failed")
	}
	return deliveryResponse(result, req.WaitSec), nil
}

func deliveryResponse(result ccmsg.Delivery, waitSec int) relay.MessageResponse {
	verdict := result.Status
	if verdict == "unconfirmed" {
		verdict = "submitted; delivery unconfirmed"
	}
	if waitSec > 0 && (result.Status == "unconfirmed" || result.Status == "delivered") {
		if result.Idle {
			verdict += "; idle confirmed"
		} else {
			verdict += "; idle not confirmed"
		}
	}
	return relay.MessageResponse{OK: result.Status == "delivered", Verdict: verdict}
}

type boundedMessageOutput struct{ bytes.Buffer }

func (b *boundedMessageOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > relay.MaxMessageBytes {
		return 0, errors.New("message helper output exceeds size limit")
	}
	return b.Buffer.Write(p)
}

// DeliverMessage is the root relay handler. Only bounded JSON crosses the UID
// boundary; no actor-provided command or socket path is ever interpreted here.
func DeliverMessage(ctx context.Context, req relay.MessageRequest) relay.MessageResponse {
	if os.Geteuid() != 0 {
		return relay.MessageResponse{Verdict: "message delivery requires guest supervisor"}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.WaitSec)*time.Second+10*time.Second)
	defer cancel()
	data, err := json.Marshal(req)
	if err != nil || len(data) > relay.MaxMessageBytes || ctx.Err() != nil {
		return relay.MessageResponse{Verdict: "invalid message delivery request"}
	}
	if _, err := ReadDeliveryRequest(bytes.NewReader(data)); err != nil {
		return relay.MessageResponse{Verdict: "invalid message delivery request"}
	}
	cmd, err := guestHelperCommand(messageHelperSpec())
	if err != nil {
		return relay.MessageResponse{Verdict: "message helper unavailable"}
	}
	output := &boundedMessageOutput{}
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return relay.MessageResponse{Verdict: "message helper unavailable"}
	}
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	defer stop()
	if err := cmd.Wait(); err != nil {
		return relay.MessageResponse{Verdict: "recipient did not confirm delivery"}
	}
	var response relay.MessageResponse
	d := json.NewDecoder(strings.NewReader(output.String()))
	d.DisallowUnknownFields()
	if err := d.Decode(&response); err != nil || response.Verdict == "" {
		return relay.MessageResponse{Verdict: "invalid message helper response"}
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return relay.MessageResponse{Verdict: "invalid message helper response"}
	}
	return response
}
