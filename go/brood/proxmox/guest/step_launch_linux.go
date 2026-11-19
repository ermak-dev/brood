//go:build linux

package guest

import (
	"context"
	"crypto/rand"
	"errors"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
)

// stepLaunch identifies one root-started actor process for a particular step.
// The broker verifies the actual Claude and helper processes separately. Even a
// live bound launch is not evidence of native admission or a completed turn.
type stepLaunch struct {
	claim        attest.Claim
	id           string
	ctx          context.Context
	cancel       context.CancelFunc
	ready        bool // Protected by Runtime.stepMu; the attestation completed.
	actorInbox   inboxProcessIdentity
	inboxHandoff *InboxHandoff // Protected by Runtime.stepMu.
}

type stepLaunchRequest struct {
	claim     attest.Claim
	request   context.Context
	authority context.Context
	inbox     bool
	applied   func() bool
	launch    *stepLaunch // Set by the runtime before replying to quiesce.
}

// NewStepLaunchWorkspaceAttestor opts the root supervisor into tracking its
// own step relaunches. It does not enable inbox transport or change PTY ownership.
func (r *Runtime) NewStepLaunchWorkspaceAttestor(cfg relay.ClientConfig, origin WorkspaceOrigin) *WorkspaceAttestor {
	a := NewWorkspaceAttestor(cfg, origin, r.Quiesce)
	a.stepRuntime = r
	if cfg.HandleInbox != nil {
		a.inboxReceiverID = cfg.InboxReceiverID
	}
	return a
}

// recordStepLaunch is called only by the runtime after the new actor starts.
// The request must still be live, but the binding's lifetime is the connection,
// not the session RPC (which is canceled even after a successful response).
func (r *Runtime) recordStepLaunch(q *stepLaunchRequest, next Process) *stepLaunch {
	var observed inboxProcessIdentity
	if actor, ok := next.(*actorProcess); ok && actor != nil && actor.cmd != nil && actor.cmd.Process != nil {
		observed, _ = inboxProcessInfo(actor.cmd.Process.Pid)
	}
	r.stepMu.Lock()
	defer r.stepMu.Unlock()
	return r.registerStepLaunch(q, observed)
}

var errHeldStepLaunchRevoked = errors.New("held inbox actor lost launch authority")

// recordHeldStepLaunch records the exact pending launch before the holder can
// accept even a queued ensure. On failure the runtime closes the blocked actor.
func (r *Runtime) recordHeldStepLaunch(ctx context.Context, q *stepLaunchRequest, next Process) (*stepLaunch, error) {
	holder, ok := next.(interface {
		ptyStartIdentity() (inboxProcessIdentity, error)
		releasePTYStart() error
	})
	if !ok {
		return nil, errors.New("inbox actor did not hold its PTY startup")
	}
	actor, err := holder.ptyStartIdentity()
	if err != nil || actor.pid <= 1 || actor.uid != 1000 || actor.image != inboxGuestProcess || actor.start == "" {
		return nil, errors.New("held inbox actor identity could not be verified")
	}
	r.stepMu.Lock()
	defer r.stepMu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	l := r.registerStepLaunch(q, actor)
	if l == nil {
		return nil, errHeldStepLaunchRevoked
	}
	refuse := func(err error) (*stepLaunch, error) {
		l.cancel()
		r.stepLaunch = nil
		return nil, err
	}
	if l.ctx.Err() != nil || q.request.Err() != nil || r.stopping() {
		return refuse(errHeldStepLaunchRevoked)
	}
	if p := r.inboxPolicy; p != nil && p.authorize != nil {
		if err := p.authorize(actor); err != nil {
			return refuse(errors.New("held inbox app parent binding could not be published"))
		}
	}
	if l.ctx.Err() != nil || q.request.Err() != nil || r.stopping() {
		return refuse(errHeldStepLaunchRevoked)
	}
	if err := holder.releasePTYStart(); err != nil {
		return refuse(errors.New("held inbox actor could not start its PTY"))
	}
	return l, nil
}

// registerStepLaunch requires stepMu. Its lifetime is the relay connection,
// not the session RPC (which ends even when attestation succeeds).
func (r *Runtime) registerStepLaunch(q *stepLaunchRequest, actor inboxProcessIdentity) *stepLaunch {
	if r.stopping() || q.request.Err() != nil || q.authority.Err() != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(q.authority)
	l := &stepLaunch{claim: q.claim, id: rand.Text(), ctx: ctx, cancel: cancel, actorInbox: actor}
	r.stepLaunch = l
	return l
}

func (r *Runtime) finishStepLaunch(l *stepLaunch, request context.Context, success bool) {
	if l == nil {
		return
	}
	r.stepMu.Lock()
	defer r.stepMu.Unlock()
	if r.stepLaunch != l {
		return // A stale completion must not activate or disturb a successor.
	}
	if !success || r.stopping() || request.Err() != nil || l.ctx.Err() != nil {
		l.cancel()
		r.stepLaunch = nil
		return
	}
	l.ready = true
}

// revokeStepLaunch cancels the current binding, or only the exact launch if
// called from that process's waiter. An ordinary relaunch never inherits it.
func (r *Runtime) revokeStepLaunch(exact *stepLaunch) {
	r.stepMu.Lock()
	defer r.stepMu.Unlock()
	if l := r.stepLaunch; l != nil && (exact == nil || l == exact) {
		l.cancel()
		r.stepLaunch = nil
	}
}
