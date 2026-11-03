//go:build linux

package ccmsg

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const maxExchangeBytes = 128 * 1024

// Delivery distinguishes a written user frame from a confirmed delivery.
// Idle only confirms the separate idle subscription, never the user message.
type Delivery struct {
	Status string
	Idle   bool
}

// Exchange sends one message and listens on a separate reverse Unix socket.
// It must run as the Claude session's unprivileged user. Session paths and keys
// are untrusted; this function refuses root before opening any socket. The
// envelope asserts bypass: managed Claude runs with --dangerously-skip-permissions,
// and a mode that differs from the receiver's is held as mode-mismatch.
// waitSec requests an authenticated idle notice; zero waits briefly for status.
// A successful write without a matching receipt is unconfirmed. It does not
// prove even queue acceptance, and must not be presented as delivered.
func Exchange(ctx context.Context, socketPath, token, name, body string, waitSec int) (Delivery, error) {
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		return Delivery{}, errors.New("session messaging must not run as root")
	}
	if err := ctx.Err(); err != nil {
		return Delivery{}, err
	}
	if waitSec < 0 || waitSec > 300 || len(body) >= maxExchangeBytes || token == "" || len(token) > 4096 {
		return Delivery{}, errors.New("invalid session message input")
	}
	if !filepath.IsAbs(socketPath) || strings.ContainsAny(socketPath, "\x00\r\n") {
		return Delivery{}, errors.New("invalid session socket path")
	}
	// Короткое имя соответствует валидации обратного адреса Claude Code.
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return Delivery{}, errors.New("cannot create callback identity")
	}
	callback := filepath.Join(filepath.Dir(socketPath), fmt.Sprintf("%d-%s.sock", os.Getpid(), hex.EncodeToString(suffix[:])))
	address := "uds:" + callback
	envelope, err := (EnvelopeBuilder{From: address, FromName: name, FromMode: "bypass"}).Build(body)
	if err != nil {
		return Delivery{}, err
	}
	msgID, err := newUUID()
	if err != nil {
		return Delivery{}, errors.New("cannot create message identity")
	}
	idleID, err := newUUID()
	if err != nil {
		return Delivery{}, errors.New("cannot create message identity")
	}
	user, err := UserFrame(envelope, "next", address, msgID)
	if err != nil {
		return Delivery{}, err
	}
	frames := []Frame{AuthFrame(token), user}
	if waitSec > 0 {
		idle, _ := ControlFrame("notify_when_idle", address, "bypass", idleID)
		frames = append(frames, idle)
	}
	data, err := EncodeNDJSON(frames)
	if err != nil || len(data) > maxExchangeBytes {
		return Delivery{}, errors.New("session message exceeds size limit")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: callback, Net: "unix"})
	if err != nil {
		return Delivery{}, errors.New("session callback unavailable")
	}
	defer listener.Close()
	timeout := 250 * time.Millisecond
	if waitSec > 0 {
		timeout = time.Duration(waitSec) * time.Second
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	stop := context.AfterFunc(exchangeCtx, func() { listener.Close() })
	defer stop()
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(exchangeCtx, "unix", socketPath)
	if err != nil {
		return Delivery{}, errors.New("session inbox unavailable")
	}
	stopWrite := context.AfterFunc(exchangeCtx, func() { c.Close() })
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := exchangeCtx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetWriteDeadline(deadline)
	_, err = c.Write([]byte(data))
	c.Close()
	stopWrite()
	if err != nil {
		return Delivery{}, errors.New("session message write failed")
	}
	deadline = time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = listener.SetDeadline(deadline)
	result := Delivery{Status: "unconfirmed"}
	for {
		incoming, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, nil
		}
		// Тот же UID, что у сессии; никакой квитанции от другого пользователя.
		_ = incoming.SetReadDeadline(minDeadline(deadline, time.Now().Add(time.Second)))
		frames := readReceiptFrames(incoming)
		incoming.Close()
		for _, frame := range frames {
			if frame["type"] != "control" {
				continue
			}
			if frame["action"] == "peer_message_status" && frame["orig_msg_id"] == msgID {
				switch frame["status"] {
				case "delivered":
					result.Status = "delivered"
				case "held", "denied", "refused", "expired", "dropped":
					result.Status = frame["status"].(string)
					// Отказ получателя Claude Code шлёт как expired с деталью refused.
					if result.Status == "expired" && frame["status_detail"] == "refused" {
						result.Status = "refused"
					}
					return result, nil
				}
			}
			if frame["action"] == "peer_idle_notice" && frame["orig_msg_id"] == idleID && frame["state"] == "idle" && waitSec > 0 {
				result.Idle = true
			}
		}
		if result.Idle || (waitSec == 0 && result.Status == "delivered") {
			return result, nil
		}
	}
}

// newUUID returns a canonical lowercase UUIDv4; Claude Code correlates receipts
// only when the message ID has canonical UUID form.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func minDeadline(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func readReceiptFrames(c *net.UnixConn) []Frame {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil
	}
	sameUser := false
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		sameUser = err == nil && cred.Uid == uint32(os.Geteuid())
	}); err != nil || !sameUser {
		return nil
	}
	scanner := bufio.NewScanner(c)
	scanner.Buffer(make([]byte, 1024), maxExchangeBytes)
	var frames []Frame
	total := 0
	for scanner.Scan() {
		total += len(scanner.Bytes()) + 1
		if total > maxExchangeBytes {
			break
		}
		var f Frame
		if json.Unmarshal(scanner.Bytes(), &f) == nil {
			frames = append(frames, f)
		}
	}
	return frames
}
