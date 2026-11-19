package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	inboxChannel        = "brood-supervisor-inbox"
	inboxProtocolHeader = "X-Brood-Inbox-Protocol"
	inboxReceiverHeader = "X-Brood-Inbox-Receiver"
	inboxProtocol       = "custody-v1"
	inboxCallTimeout    = 15 * time.Second
)

// ErrInboxUnsupported is a pre-open refusal, not a legacy retry permission.
var ErrInboxUnsupported = errors.New("guest relay does not support supervisor inbox custody")

// ErrInboxReceiverChanged refuses to retarget an existing intent to new storage.
var ErrInboxReceiverChanged = errors.New("guest supervisor inbox identity changed")

func inboxAdvertisement(header http.Header) (string, error) {
	versions := header.Values(inboxProtocolHeader)
	receivers := header.Values(inboxReceiverHeader)
	if len(versions) == 0 && len(receivers) == 0 {
		return "", nil
	}
	if len(versions) != 1 || versions[0] != inboxProtocol || len(receivers) != 1 || !validMessageJournalID(receivers[0]) {
		return "", errInboxFrame
	}
	return receivers[0], nil
}

// InboxReceiverID reports an observation from the current authenticated peer.
// CallInbox rechecks the same identity at channel open.
func (h *Hub) InboxReceiverID(uid string, incarnation int64) string {
	if selected := h.session(uid, incarnation); selected != nil {
		return selected.inboxReceiverID
	}
	return ""
}

// CallInbox sends a single bounded custody request, never a native peer message.
// Authority to send an instruction must be checked by the caller before invoking it.
func (h *Hub) CallInbox(parent context.Context, request InboxRequest) (InboxResponse, error) {
	ctx, cancel := context.WithTimeout(parent, inboxCallTimeout)
	defer cancel()
	stream, err := h.DialInbox(ctx, request)
	if err != nil {
		return InboxResponse{}, err
	}
	defer stream.Close()
	response, err := ReadInboxResponse(stream, request.Target)
	if err != nil {
		return InboxResponse{}, err
	}
	if err := ValidateInboxDisposition(request.Operation, response.Disposition); err != nil {
		return InboxResponse{}, err
	}
	return response, nil
}

// DialInbox opens a bounded typed custody request after the caller checks
// submission authority. The connection follows ctx; keep it bounded. The
// caller can release its effect gate before reading the guest response.
func (h *Hub) DialInbox(ctx context.Context, request InboxRequest) (net.Conn, error) {
	body, err := EncodeInboxRequest(request)
	if err != nil {
		return nil, err
	}
	target := request.Target
	return h.dialScopedMessage(ctx, target.ActorUID, target.Incarnation, body, inboxChannel, func(p *peer) error {
		if p.inboxReceiverID == "" {
			return ErrInboxUnsupported
		}
		if p.inboxReceiverID != target.ReceiverID {
			return ErrInboxReceiverChanged
		}
		return nil
	})
}

// ReadInboxResponse reads only a bounded receipt for the exact request target.
func ReadInboxResponse(stream io.Reader, target InboxTarget) (InboxResponse, error) {
	frame, err := io.ReadAll(io.LimitReader(stream, MaxMessageBytes+1))
	if err != nil {
		return InboxResponse{}, errUnavailable
	}
	return DecodeInboxResponse(frame, target)
}

func serveGuestInbox(parent context.Context, handler func(context.Context, []byte) ([]byte, error), ch ssh.NewChannel) {
	frame := ch.ExtraData()
	request, err := DecodeInboxRequest(frame)
	if err != nil {
		ch.Reject(ssh.Prohibited, "invalid supervisor inbox frame")
		return
	}
	ctx, cancel := context.WithTimeout(parent, inboxCallTimeout)
	defer cancel()
	stream, requests, err := ch.Accept()
	if err != nil {
		return
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	go ssh.DiscardRequests(requests)
	go func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(stream, 1))
		cancel()
	}()
	response, err := handler(ctx, frame)
	if err != nil || ctx.Err() != nil {
		return
	}
	decoded, err := DecodeInboxResponse(response, request.Target)
	if err != nil || ValidateInboxDisposition(request.Operation, decoded.Disposition) != nil {
		return
	}
	_, _ = stream.Write(response)
}
