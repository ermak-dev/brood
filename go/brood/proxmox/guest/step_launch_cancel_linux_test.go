//go:build linux

package guest

import (
	"context"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

// BUG: Relay cancellation during a gated relaunch must not stop the guest.
// HYPOTHESIS: Loss of launch authority was treated as a broken actor process.
func TestInboxPTYStartGateRestoresOrdinaryActorAfterConnectionLoss(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	connection, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	var held *heldPTYTestProcess
	withHeldPTYTestLauncher(c, func(p *heldPTYTestProcess) {
		held = p
		p.identify = func() error { disconnect(); return nil }
	})
	r, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a := inboxPTYTestAttestor(r, &attestRecorder{})
	a.Ensure = func(context.Context) error {
		t.Error("canceled inbox attestation started a Claude app")
		return nil
	}
	resp := a.attest(context.Background(), connection, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil)})
	if resp.OK || resp.InboxLaunch != nil || held == nil || held.serving || held.releaseCalls != 0 || boundActorLaunch(r) != nil {
		t.Fatalf("canceled launch became usable: %+v", resp)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Quiesce(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("lost relay authority stopped the guest runtime instead of restoring an ordinary actor: %v", err)
	}
}

// Cancel just after the caller checks Err; the context observed by the launch
// constructor already has a closed Done channel.
type cancelAfterObservedAuthority struct {
	context.Context
	cancel context.CancelFunc
	seen   bool
}

func (c *cancelAfterObservedAuthority) Err() error {
	if !c.seen {
		c.seen = true
		c.cancel()
		return nil
	}
	return c.Context.Err()
}

func TestInboxPTYStartGateRejectsAlreadyCanceledBinding(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	authority := &cancelAfterObservedAuthority{Context: parent, cancel: cancel}
	r := &Runtime{stop: make(chan struct{})}
	q := &stepLaunchRequest{request: context.Background(), authority: authority}
	held := &heldPTYTestProcess{
		fakeProcess: &fakeProcess{done: make(chan struct{})},
		identity:    inboxProcessIdentity{pid: 202, parent: 1, start: "8080", uid: 1000, image: inboxGuestProcess},
	}
	if launch, err := r.recordHeldStepLaunch(context.Background(), q, held); err == nil || launch != nil || held.releaseCalls != 0 || r.stepLaunch != nil {
		t.Fatalf("lost authority released the held PTY: launch=%t err=%v releases=%d", launch != nil, err, held.releaseCalls)
	}
}
