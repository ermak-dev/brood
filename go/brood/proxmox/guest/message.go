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

	"brood.dev/go/brood/command"
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

// actorSessions — каталог файлов сессий Claude актора; читает его только
// helper под UID 1000.
const actorSessions = "/workspace/home/.claude/sessions"

// ReceiveMessage runs only inside the fixed, unprivileged delivery helper.
// Paths from actor-owned session files are never opened by the root supervisor.
// Refusals before the session inbox write are NotSent (#244). Without an idle
// wait the helper waits up to command.ReceiptWait for the delivery receipt.
func ReceiveMessage(ctx context.Context, req relay.MessageRequest) relay.MessageResponse {
	return messageReceiver{identity: messageHelperIdentity, session: liveProjectSession, token: func(socket string) (string, error) {
		return ccmsg.PeerToken(socket, actorSessions)
	}, exchange: ccmsg.Exchange}.receive(ctx, req)
}

// messageReceiver — шаги доставки helper-а по порядку. Отказ любого шага до
// записи в inbox сессии — NotSent: сообщение заведомо не ушло, и сервер может
// повторить доставку, не рискуя дублем.
type messageReceiver struct {
	identity func() error
	session  func(context.Context) (ccmsg.Session, error)
	token    func(socket string) (string, error)
	exchange func(ctx context.Context, socket, token, from, body string, waitSec int, receiptWait time.Duration) (ccmsg.Delivery, error)
}

func (m messageReceiver) receive(ctx context.Context, req relay.MessageRequest) relay.MessageResponse {
	if err := m.identity(); err != nil {
		return messageNotSent(err.Error())
	}
	// Сессии нет и сразу после перезапуска сессии (restart):
	// новый Claude ещё не записал файл сессии и не слушает inbox.
	session, err := m.session(ctx)
	if err != nil {
		return messageNotSent("no unique live Claude session for this project")
	}
	token, err := m.token(session.SocketPath)
	if err != nil {
		return messageNotSent("Claude session peer key unavailable")
	}
	result, err := m.exchange(ctx, session.SocketPath, token, req.From, req.Body, req.WaitSec, command.ReceiptWait)
	switch {
	case errors.Is(err, ccmsg.ErrNotSent):
		return messageNotSent("Claude session message was not sent")
	case err != nil:
		return relay.MessageResponse{Verdict: "Claude session message exchange failed"}
	}
	return deliveryResponse(result, req.WaitSec)
}

func messageNotSent(verdict string) relay.MessageResponse {
	return relay.MessageResponse{Verdict: verdict, NotSent: true}
}

// messageHelperIdentity — helper ровно UID/GID 1000 без supplementary groups
// и со сброшенными привилегиями до чтения файлов сессий.
func messageHelperIdentity() error {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return errors.New("message helper requires UID and GID 1000")
	}
	groups, err := os.Getgroups()
	if err != nil {
		return errors.New("message helper group check failed")
	}
	for _, g := range groups {
		if g != 1000 {
			return errors.New("message helper has unexpected supplementary groups")
		}
	}
	return lockPrivileges()
}

// liveProjectSession — единственная сессия /workspace/project с живым inbox.
func liveProjectSession(ctx context.Context) (ccmsg.Session, error) {
	return ccmsg.SessionForCWD(actorSessions, "/workspace/project", func(path string) bool {
		probe, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
		if err != nil {
			return false
		}
		probe.Close()
		return true
	})
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

// helperTimeout — сколько supervisor ждёт helper доставки: ожидание
// квитанции или простоя (command.ReplyWait), запись Exchange
// (ccmsg.WriteTimeout) и запас на запуск helper-а и поиск сессии. Предел
// короче command.CallTimeout, которым relay гостя держит канал: вердикт
// успевает уйти серверу (#260).
func helperTimeout(waitSec int) time.Duration {
	return command.ReplyWait(waitSec) + 10*time.Second
}

// DeliverMessage is the root relay handler. Only bounded JSON crosses the UID
// boundary; no actor-provided command or socket path is ever interpreted here.
// Refusals before the helper starts are NotSent; once it runs, only its own
// answer may say so.
func DeliverMessage(ctx context.Context, req relay.MessageRequest) relay.MessageResponse {
	if os.Geteuid() != 0 {
		return messageNotSent("message delivery requires guest supervisor")
	}
	if req.DeliveryID != "" || req.JournalID != "" || req.Replay {
		return messageNotSent("durable delivery requires the negotiated journal handler")
	}
	ctx, cancel := context.WithTimeout(ctx, helperTimeout(req.WaitSec))
	defer cancel()
	data, err := json.Marshal(req)
	if err != nil || len(data) > relay.MaxMessageBytes || ctx.Err() != nil {
		return messageNotSent("invalid message delivery request")
	}
	if _, err := ReadDeliveryRequest(bytes.NewReader(data)); err != nil {
		return messageNotSent("invalid message delivery request")
	}
	cmd, err := guestHelperCommand(messageHelperSpec())
	if err != nil {
		return messageNotSent("message helper unavailable")
	}
	output := &boundedMessageOutput{}
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return messageNotSent("message helper unavailable")
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
