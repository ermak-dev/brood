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
	"time"

	"brood.dev/go/brood/proxmox/relay"
	"golang.org/x/sys/unix"
)

const inboxHelperRequest = "{\"operation\":\"take\"}\n"
const inboxHelperSocket = "/run/brood/supervisor-inbox.sock"
const inboxHelperWaiting = "{\"state\":\"waiting\"}\n"

// InboxConsumerAuthenticator resolves the current launch from a trusted local
// process/session identity. Neither a claimed field nor UID equality alone is
// sufficient. The supervisor must supply this with its lifecycle fence held.
type InboxConsumerAuthenticator func(context.Context, *net.UnixConn) (string, error)

// InboxHelperOffer carries already claimed work for structured prompt intake.
// It exposes no permit, does not confirm native admission, and is not a result.
type InboxHelperOffer struct {
	DeliveryID string `json:"deliveryId"`
	Text       string `json:"text"`
}

// RunInboxHelper is the fixed, unprivileged app entrypoint. Its socket path
// and request are not supplied by the actor. Without a trusted active broker
// it fails without reading supervisor configuration or using legacy messaging.
func RunInboxHelper(parent context.Context, input io.Reader, output io.Writer) error {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return ErrInboxHandoffUnavailable
	}
	if err := lockPrivileges(); err != nil {
		return ErrInboxHandoffUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(input, int64(len(inboxHelperRequest)+1)))
	if err != nil || !bytes.Equal(data, []byte(inboxHelperRequest[:len(inboxHelperRequest)-1])) {
		return ErrInboxHandoffUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	dialed, err := (&net.Dialer{}).DialContext(ctx, "unix", inboxHelperSocket)
	if err != nil {
		return ErrInboxHandoffUnavailable
	}
	defer dialed.Close()
	connection, ok := dialed.(*net.UnixConn)
	if !ok {
		return ErrInboxHandoffUnavailable
	}
	credentials, err := inboxHelperCredentials(connection)
	if err != nil || credentials == nil || credentials.Uid != 0 {
		return ErrInboxHandoffUnavailable
	}
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return ErrInboxHandoffUnavailable
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.SetDeadline(time.Now()) })
	defer stop()
	if n, err := io.WriteString(connection, inboxHelperRequest); err != nil || n != len(inboxHelperRequest) || connection.CloseWrite() != nil {
		return ErrInboxHandoffUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(connection, relay.MaxMessageBytes+1))
	if err == nil && ctx.Err() == nil && bytes.Equal(raw, []byte(inboxHelperWaiting)) {
		return ErrInboxHandoffWaiting
	}
	var offer InboxHelperOffer
	if err != nil || len(raw) > relay.MaxMessageBytes || json.Unmarshal(raw, &offer) != nil || !validHandoffID(offer.DeliveryID) || offer.Text == "" || ctx.Err() != nil {
		return ErrInboxHandoffUnavailable
	}
	canonical, err := json.Marshal(offer)
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) {
		return ErrInboxHandoffUnavailable
	}
	if n, err := output.Write(raw); err != nil || n != len(raw) {
		return ErrInboxHandoffUnavailable
	}
	return nil
}

func inboxHelperCredentials(connection *net.UnixConn) (*unix.Ucred, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var failure error
	if err := raw.Control(func(fd uintptr) {
		cred, failure = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, failure
}

// ServeInboxHelperConnection reads one fixed request on an already accepted
// local stream. The caller owns connection and must close it after returning.
// No actor input selects an inbox, launch, permit, message, or command.
func ServeInboxHelperConnection(parent context.Context, connection *net.UnixConn, handoff *InboxHandoff, authenticate InboxConsumerAuthenticator) error {
	if connection == nil || handoff == nil {
		return ErrInboxHandoffUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return ErrInboxHandoffUnavailable
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.SetDeadline(time.Now()) })
	defer stop()
	data, err := io.ReadAll(io.LimitReader(connection, int64(len(inboxHelperRequest)+1)))
	if err != nil || !bytes.Equal(data, []byte(inboxHelperRequest)) || authenticate == nil || ctx.Err() != nil {
		return ErrInboxHandoffUnavailable
	}
	launch, err := authenticate(ctx, connection)
	var work InboxWork
	if err == nil {
		work, err = handoff.Take(ctx, launch)
	}
	if errors.Is(err, ErrInboxHandoffWaiting) && ctx.Err() == nil && handoff.launch.Err() == nil {
		if n, err := io.WriteString(connection, inboxHelperWaiting); err == nil && n == len(inboxHelperWaiting) {
			return nil
		}
		return ErrInboxHandoffUnavailable
	}
	if err != nil {
		return ErrInboxHandoffUnavailable
	}
	// Only the canonical server-created prompt payload crosses to the mod.
	// Re-encoding forbids alias keys, duplicates and legacy control metadata.
	var message relay.MessageRequest
	if json.Unmarshal(work.Payload, &message) != nil || message.Body == "" || message.To == "" || message.From == "" || message.WaitSec != 0 || message.DeliveryID != "" || message.JournalID != "" || message.Replay {
		return ErrInboxHandoffUnavailable
	}
	canonical, err := json.Marshal(message)
	if err != nil || !bytes.Equal(canonical, work.Payload) {
		return ErrInboxHandoffUnavailable
	}
	response, err := json.Marshal(InboxHelperOffer{DeliveryID: work.Ticket.Receipt.DeliveryID, Text: message.Body})
	if err != nil || len(response)+1 > relay.MaxMessageBytes || ctx.Err() != nil || handoff.launch.Err() != nil {
		return ErrInboxHandoffUnavailable
	}
	if n, err := connection.Write(append(response, '\n')); err != nil || n != len(response)+1 {
		return ErrInboxHandoffUnavailable
	}
	return nil
}
