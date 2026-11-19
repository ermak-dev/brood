//go:build linux

package guest

import (
	"context"
	"net"
	"os"
	"sync"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/relay"
)

// StepInboxBroker owns the supervisor socket and private inbox. It serves only
// the exact current step launch; the actor never selects a delivery or permit.
type StepInboxBroker struct {
	runtime *Runtime
	inbox   *deliverylog.Inbox
	socket  *actorSocket
	ctx     context.Context
	cancel  context.CancelFunc
	closed  chan struct{}
	once    sync.Once
	err     error
}

// StartStepInboxBroker requires a deliberately selected actor runtime and
// private receiving directory. It does not publish managed settings.
func (r *Runtime) StartStepInboxBroker(parent context.Context, directory string, scope deliverylog.Scope) (*StepInboxBroker, error) {
	if os.Geteuid() != 0 || parent.Err() != nil || r.stopping() || prepareRunDirectory() != nil {
		return nil, ErrInboxHandoffUnavailable
	}
	inbox, err := OpenSupervisorInbox(parent, directory, scope)
	if err != nil {
		return nil, err
	}
	socket, err := createActorSocket(inboxHelperSocket, 0, 1000)
	if err != nil {
		_ = inbox.Close()
		return nil, ErrInboxHandoffUnavailable
	}
	ctx, cancel := context.WithCancel(parent)
	b := &StepInboxBroker{runtime: r, inbox: inbox, socket: socket, ctx: ctx, cancel: cancel, closed: make(chan struct{})}
	go b.serve()
	return b, nil
}

// ConfigureRelay advertises custody only on the connection of the exact
// scoped actor. Call after the managed bundle and helper are ready to use.
func (b *StepInboxBroker) ConfigureRelay(config *relay.ClientConfig) error {
	if config == nil || b.ctx.Err() != nil || b.inbox.ActorScope() != (deliverylog.Scope{ActorUID: config.ActorUID, Incarnation: config.Generation}) {
		return ErrInboxHandoffUnavailable
	}
	config.InboxReceiverID = b.inbox.Identifier()
	config.HandleInbox = SupervisorInboxWithDispatch(b.inbox, func(ctx context.Context, id string) *InboxHandoff {
		if ctx.Err() != nil {
			return nil
		}
		_, handoff := b.binding(id)
		return handoff
	})
	return nil
}

func (b *StepInboxBroker) binding(expectedID string) (*stepLaunch, *InboxHandoff) {
	r := b.runtime
	r.stepMu.Lock()
	defer r.stepMu.Unlock()
	l := r.stepLaunch
	if b.ctx.Err() != nil || l == nil || l.ctx.Err() != nil || r.stopping() || l.actorInbox.pid == 0 ||
		l.claim.ActorUID != b.inbox.ActorScope().ActorUID || l.claim.Generation != b.inbox.ActorScope().Incarnation {
		return nil, nil
	}
	if expectedID != "" && (!l.ready || l.id != expectedID) {
		return nil, nil
	}
	if l.inboxHandoff == nil {
		handoff, err := NewInboxHandoff(l.ctx, b.inbox, l.id)
		if err != nil {
			return nil, nil
		}
		l.inboxHandoff = handoff
	}
	if l.inboxHandoff.inbox != b.inbox {
		return nil, nil
	}
	return l, l.inboxHandoff
}

func (b *StepInboxBroker) serveConn(connection *net.UnixConn) {
	defer connection.Close()
	l, handoff := b.binding("")
	if handoff == nil {
		return
	}
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	_ = ServeInboxHelperConnection(ctx, connection, handoff, func(ctx context.Context, c *net.UnixConn) (string, error) {
		return b.runtime.authenticateInboxConsumer(ctx, c, l)
	})
}

func (b *StepInboxBroker) serve() {
	defer close(b.closed)
	defer b.cancel()
	stop := context.AfterFunc(b.ctx, func() { _ = b.socket.Close() })
	defer stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	slots := make(chan struct{}, relay.MaxMessagesInFlight)
	for b.ctx.Err() == nil {
		connection, err := b.socket.listener.AcceptUnix()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
			workers.Go(func() { defer func() { <-slots }(); b.serveConn(connection) })
		default:
			_ = connection.Close()
		}
	}
}

// Close revokes the socket and waits for currently bound readers. The inbox
// on disk remains intact and no closed runtime grants authority to replay.
func (b *StepInboxBroker) Close() error {
	b.once.Do(func() {
		b.cancel()
		_ = b.socket.Close()
		<-b.closed
		b.err = b.inbox.Close()
	})
	return b.err
}
