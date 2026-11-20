//go:build linux

package guest

// FEATURE: Supervisor-owned actor launch binding for a workspace step (#291).
// SPEC: Only a completed attestation binds the exact post-quiesce actor launch.
// Replacement, disconnect, request cancellation before completion, and
// actor shutdown cannot retain or transfer the binding. It is not a receipt.
// IMPLEMENTATION: The runtime binds its actual replacement Process and the
// attestor marks that binding ready only on completion.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

var stepLaunchTestConfig = relay.ClientConfig{ActorUID: "actor-1", Generation: 7, Token: attestToken}

func boundActorLaunch(r *Runtime) *stepLaunch {
	r.stepMu.Lock()
	defer r.stepMu.Unlock()
	if l := r.stepLaunch; l != nil && l.ready && l.ctx.Err() == nil {
		return l
	}
	return nil
}

func stepLaunchTestAttestor(r *Runtime, rec *attestRecorder) *WorkspaceAttestor {
	a := r.NewStepLaunchWorkspaceAttestor(stepLaunchTestConfig, WorkspaceOrigin{Origin: attestOrigin, Mode: "gateway"})
	effects := rec.attestor()
	a.Checkout, a.Step, a.Free, a.Ensure = effects.Checkout, effects.Step, effects.Free, effects.Ensure
	return a
}

func TestStepLaunchBindsOnlyTheAttestedActorRelaunch(t *testing.T) {
	var events []string
	c := testConfig(&events)
	r, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if boundActorLaunch(r) != nil {
		t.Fatal("the initial free actor acquired a step launch binding")
	}
	a := stepLaunchTestAttestor(r, &attestRecorder{})
	checkout := a.Checkout
	a.Checkout = func(ctx context.Context, req sessionctl.CheckoutRequest) (sessionctl.CheckoutFacts, error) {
		if boundActorLaunch(r) != nil || events[len(events)-1] != "process-close" {
			t.Error("checkout ran before the old actor launch was stopped and revoked")
		}
		return checkout(ctx, req)
	}
	a.Ensure = func(context.Context) error {
		if boundActorLaunch(r) != nil || events[len(events)-1] != "process-start" {
			t.Error("launch was not pending after replacement or was bound before session ensure")
		}
		return nil
	}
	connection, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	handle := NewSupervisorSessionHandler(connection, nil, a)
	call := func(args json.RawMessage) relay.SessionResponse {
		request, cancel := context.WithCancel(connection)
		defer cancel() // Normal completion closes the request, not the connection.
		return handle(request, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: args})
	}
	resp := call(attestArgs(nil))
	first := boundActorLaunch(r)
	want := attest.Claim{ActorUID: "actor-1", Generation: 7, Swarm: "01JSWARM0000000000000000AA", Step: "request-1", Origin: attestOrigin, Commit: attestCommit, Challenge: attestChallenge}
	if !resp.OK || first == nil || first.claim != want || !validHandoffID(first.id) {
		t.Fatalf("did not bind exactly the requested step to the root relaunch: %+v", resp)
	}
	if strings.Contains(resp.Text, first.id) {
		t.Fatal("launch ID leaked into the workspace receipt")
	}
	if resp := call(attestArgs(map[string]any{"launchId": first.id})); resp.OK || boundActorLaunch(r) != first {
		t.Fatal("a provided launch ID was accepted or changed the current binding")
	}
	resp = call(attestArgs(map[string]any{"step": "request-2"}))
	second := boundActorLaunch(r)
	want.Step = "request-2"
	if !resp.OK || first.ctx.Err() == nil || second == nil || second.id == first.id || second.claim != want {
		t.Fatalf("replacing the step transferred the old launch or did not bind the new one: %+v", resp)
	}
	if err := r.Quiesce(context.Background(), func(context.Context) error {
		if second.ctx.Err() == nil || boundActorLaunch(r) != nil {
			return errors.New("launch remained bound during ordinary quiesce")
		}
		return nil
	}); err != nil || boundActorLaunch(r) != nil {
		t.Fatalf("ordinary quiesce transferred a binding: %v", err)
	}
}

func TestStepLaunchRefusesIncompleteTransitions(t *testing.T) {
	for _, effect := range []string{"checkout", "step", "ensure", "relaunch", "request canceled", "connection canceled", "superseded"} {
		t.Run(effect, func(t *testing.T) {
			var events []string
			c := testConfig(&events)
			if effect == "relaunch" {
				launch := c.Dependencies.Launch
				c.Dependencies.Launch = func(ctx context.Context, spec ActorSpec) (Process, error) {
					if events[len(events)-1] == "process-close" {
						return nil, errors.New("cannot relaunch")
					}
					return launch(ctx, spec)
				}
			}
			r, err := Start(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			a := stepLaunchTestAttestor(r, &attestRecorder{fail: map[string]error{effect: errors.New("effect failed")}})
			connection, disconnect := context.WithCancel(context.Background())
			defer disconnect()
			request, cancel := context.WithCancel(connection)
			defer cancel()
			if effect == "request canceled" || effect == "connection canceled" {
				a.Ensure = func(ctx context.Context) error {
					if effect == "request canceled" {
						cancel()
					} else {
						disconnect()
					}
					return ctx.Err() // Detached restoration should finish normally.
				}
			}
			if effect == "superseded" {
				a.Ensure = func(ctx context.Context) error {
					return r.Quiesce(ctx, func(context.Context) error { return nil })
				}
			}
			handle := NewSupervisorSessionHandler(connection, nil, a)
			resp := handle(request, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil)})
			if effect == "request canceled" || effect == "connection canceled" || effect == "superseded" {
				if !resp.OK {
					t.Fatalf("launch revocation changed legacy workspace restoration: %+v", resp)
				}
			} else if resp.OK {
				t.Fatalf("incomplete transition succeeded: %+v", resp)
			}
			if boundActorLaunch(r) != nil {
				t.Fatal("incomplete transition bound a launch")
			}
		})
	}
}

func TestStepLaunchRevokesOnLossOfAuthority(t *testing.T) {
	for _, effect := range []string{"disconnect", "runtime stop", "actor exit", "restart", "recycle", "partial policy clear"} {
		t.Run(effect, func(t *testing.T) {
			var events []string
			c := testConfig(&events)
			var process Process
			launch := c.Dependencies.Launch
			c.Dependencies.Launch = func(ctx context.Context, spec ActorSpec) (Process, error) {
				p, err := launch(ctx, spec)
				process = p
				return p, err
			}
			r, err := Start(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			connection, disconnect := context.WithCancel(context.Background())
			defer disconnect()
			a := stepLaunchTestAttestor(r, &attestRecorder{})
			var bound *stepLaunch
			handle := newSessionHandler(connection, nil, a, func(context.Context, relay.SessionRequest) relay.SessionResponse {
				if effect == "partial policy clear" {
					t.Error("replacement reached the helper after a policy failure")
				}
				if bound.ctx.Err() == nil || boundActorLaunch(r) != nil {
					t.Error("session replacement reached the helper before binding revocation")
				}
				return relay.SessionResponse{OK: true, Outcome: "complete"}
			})
			resp := handle(connection, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil)})
			bound = boundActorLaunch(r)
			if !resp.OK || bound == nil {
				t.Fatalf("could not create initial binding: %+v", resp)
			}
			switch effect {
			case "disconnect":
				disconnect()
			case "runtime stop":
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			case "actor exit":
				if err := process.Close(); err != nil {
					t.Fatal(err)
				}
				if err := r.Wait(); err != nil {
					t.Fatal(err)
				}
			case "partial policy clear":
				a.Free = func() error {
					if bound.ctx.Err() == nil {
						t.Error("policy was changed before revocation")
					}
					return errors.New("managed settings could not be removed after clearing the step marker")
				}
				if resp := handle(connection, relay.SessionRequest{Operation: "restart", Arguments: json.RawMessage(`{"confirm":true}`)}); resp.OK {
					t.Fatal("partial policy clear allowed a restart")
				}
			default:
				resp := handle(connection, relay.SessionRequest{Operation: effect, Arguments: json.RawMessage(`{"confirm":true}`)})
				if !resp.OK {
					t.Fatalf("replacement denied: %+v", resp)
				}
			}
			if bound.ctx.Err() == nil || boundActorLaunch(r) != nil {
				t.Fatal("old launch authority survived")
			}
		})
	}
}

// The launcher returns a blocked holder for ActorSpec.ptyStartHeld(). Its
// process can report its live identity and only the lifecycle can release it.
// Process.Close must also work before release. No real child or socket is used.
type heldPTYTestProcess struct {
	*fakeProcess
	identity     inboxProcessIdentity
	identify     func() error
	identified   bool
	beforeServe  func() error
	releaseCalls int
	serving      bool
}

func (p *heldPTYTestProcess) ptyStartIdentity() (inboxProcessIdentity, error) {
	p.identified = true
	select {
	case <-p.done:
		return inboxProcessIdentity{}, errors.New("held actor already exited")
	default:
	}
	if p.identify != nil {
		if err := p.identify(); err != nil {
			return inboxProcessIdentity{}, err
		}
	}
	return p.identity, nil
}

func (p *heldPTYTestProcess) releasePTYStart() error {
	p.releaseCalls++
	if p.beforeServe != nil {
		if err := p.beforeServe(); err != nil {
			return err
		}
	}
	p.serving = true
	return nil
}

func withHeldPTYTestLauncher(c Config, onHeld func(*heldPTYTestProcess)) {
	launch := c.Dependencies.Launch
	c.Dependencies.Launch = func(ctx context.Context, spec ActorSpec) (Process, error) {
		p, err := launch(ctx, spec)
		// Treat absence of the opt-in accessor as the baseline ungated behavior,
		// so these tests fail on behavior rather than on a missing declaration.
		held, ok := any(spec).(interface{ ptyStartHeld() bool })
		if err != nil || !ok || !held.ptyStartHeld() {
			return p, err
		}
		pg := &heldPTYTestProcess{
			fakeProcess: p.(*fakeProcess),
			identity:    inboxProcessIdentity{pid: 202, parent: 1, start: "8080", uid: 1000, image: inboxGuestProcess},
		}
		onHeld(pg)
		return pg, nil
	}
}

func inboxPTYTestAttestor(r *Runtime, rec *attestRecorder) *WorkspaceAttestor {
	cfg := stepLaunchTestConfig
	cfg.InboxReceiverID = strings.Repeat("a", 64)
	cfg.HandleInbox = func(context.Context, []byte) ([]byte, error) {
		return nil, errors.New("inbox transport is not part of this test")
	}
	origin := WorkspaceOrigin{Origin: attestOrigin, Mode: "gateway", Ephemeral: &EphemeralWorkspace{Commit: attestCommit}}
	a := r.NewStepLaunchWorkspaceAttestor(cfg, origin)
	effects := rec.attestor()
	a.Checkout, a.Step, a.Free, a.Ensure = effects.Checkout, effects.Step, effects.Free, effects.Ensure
	a.Prepare = func(context.Context, sessionctl.CheckoutRequest) error { return nil }
	return a
}

func TestInboxPTYStartGateRecordsExactLaunchBeforeServing(t *testing.T) {
	var events []string
	c := testConfig(&events)
	var r *Runtime
	var held *heldPTYTestProcess
	var pending *stepLaunch
	want := attest.Claim{ActorUID: "actor-1", Generation: 7, Swarm: "01JSWARM0000000000000000AA", Step: "request-1", Origin: attestOrigin, Commit: attestCommit, Challenge: attestChallenge}
	withHeldPTYTestLauncher(c, func(p *heldPTYTestProcess) {
		held = p
		p.beforeServe = func() error {
			// This callback runs synchronously on the lifecycle goroutine, at
			// the moment a queued ensure could first be served. Do not take
			// stepMu here: the lifecycle is allowed to hold it during release.
			if r == nil {
				return errors.New("initial actor requested a gated start")
			}
			pending = r.stepLaunch
			if pending == nil || pending.actorInbox != p.identity || pending.claim != want || pending.ready || pending.ctx.Err() != nil {
				t.Error("holder was released without the exact live, pending launch")
				return errors.New("pending launch is missing at release")
			}
			return nil
		}
	})
	var err error
	r, err = Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if held != nil {
		t.Fatal("initial actor was gated")
	}
	a := inboxPTYTestAttestor(r, &attestRecorder{})
	ensures := 0
	a.Ensure = func(context.Context) error {
		ensures++
		r.stepMu.Lock()
		live := pending != nil && r.stepLaunch == pending && !pending.ready && pending.ctx.Err() == nil
		r.stepMu.Unlock()
		if held == nil || !held.identified || !held.serving || held.releaseCalls != 1 || !live {
			t.Error("first ensure did not reach a released holder with a pending launch")
			return errors.New("holder is not ready to serve the first ensure")
		}
		return nil
	}
	resp := attestCall(a, attestArgs(nil))
	if !resp.OK || pending == nil || resp.InboxLaunch == nil || resp.InboxLaunch.LaunchID != pending.id || boundActorLaunch(r) != pending || ensures != 1 {
		t.Fatalf("attestation did not complete on the launch released for its first ensure: %+v (ensures: %d)", resp, ensures)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Wait(); err != nil {
		t.Fatal(err)
	}
	if pending.ctx.Err() == nil || boundActorLaunch(r) != nil {
		t.Fatal("the released holder exited without revoking its launch")
	}
}

func TestInboxPTYStartGateDoesNotServeOnFailedBinding(t *testing.T) {
	for _, failure := range []string{"identity unavailable", "request canceled", "connection canceled", "release failed"} {
		t.Run(failure, func(t *testing.T) {
			var events []string
			c := testConfig(&events)
			connection, disconnect := context.WithCancel(context.Background())
			defer disconnect()
			// Keep the two lifetimes independent to exercise each check.
			request, cancel := context.WithCancel(context.Background())
			defer cancel()
			var r *Runtime
			var held *heldPTYTestProcess
			var pending *stepLaunch
			withHeldPTYTestLauncher(c, func(p *heldPTYTestProcess) {
				held = p
				p.identify = func() error {
					switch failure {
					case "identity unavailable":
						return errors.New("holder identity unavailable")
					case "request canceled":
						cancel()
					case "connection canceled":
						disconnect()
					}
					return nil
				}
				p.beforeServe = func() error {
					if failure == "release failed" {
						pending = r.stepLaunch
						return errors.New("cannot release held actor")
					}
					return nil
				}
			})
			var err error
			r, err = Start(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if held != nil {
				t.Fatal("initial actor was gated")
			}
			a := inboxPTYTestAttestor(r, &attestRecorder{})
			ensures := 0
			a.Ensure = func(context.Context) error { ensures++; return nil }
			resp := a.attest(request, connection, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil)})
			if resp.OK || resp.InboxLaunch != nil || ensures != 0 || held == nil {
				t.Fatalf("failed gate reached ensure or did not attempt a held replacement: %+v (ensures: %d, held: %t)", resp, ensures, held != nil)
			}
			if !held.identified {
				t.Fatal("runtime never checked the gated holder identity")
			}
			if failure == "release failed" {
				if held.releaseCalls != 1 || pending == nil || pending.ctx.Err() == nil {
					t.Fatal("failed release did not revoke the recorded pending launch")
				}
			} else if held.releaseCalls != 0 {
				t.Fatal("holder was released without a live binding")
			}
			r.stepMu.Lock()
			active := r.stepLaunch
			r.stepMu.Unlock()
			if held.serving || active != nil {
				t.Fatal("failed gate left a serving holder or a recorded launch")
			}
			select {
			case <-held.done:
			case <-time.After(5 * time.Second):
				t.Fatal("failed gate left the blocked replacement running")
			}
		})
	}
}

func TestInboxPTYStartGateLeavesOrdinaryRelaunchUngated(t *testing.T) {
	for _, kind := range []string{"tracking only", "ordinary persistent", "persistent restoration", "result observation"} {
		t.Run(kind, func(t *testing.T) {
			var events []string
			c := testConfig(&events)
			held := false
			withHeldPTYTestLauncher(c, func(*heldPTYTestProcess) { held = true })
			r, err := Start(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			rec := &attestRecorder{}
			a := inboxPTYTestAttestor(r, rec)
			switch kind {
			case "tracking only":
				tracking := stepLaunchTestAttestor(r, rec)
				tracking.Origin, tracking.Prepare = a.Origin, a.Prepare
				a = tracking
			case "ordinary persistent":
				a = NewWorkspaceAttestor(stepLaunchTestConfig, WorkspaceOrigin{Origin: attestOrigin, Mode: "gateway"}, r.Quiesce)
				effects := rec.attestor()
				a.Checkout, a.Step, a.Free = effects.Checkout, effects.Step, effects.Free
			case "persistent restoration":
				a.Origin.Ephemeral = nil
				rec.fail = map[string]error{"checkout": errors.New("checkout refused")}
			}
			ensures := 0
			a.Ensure = func(context.Context) error { ensures++; return nil }
			var resp relay.SessionResponse
			if kind == "result observation" {
				a.Observe = func(_ context.Context, req sessionctl.CheckoutRequest) (sessionctl.ResultFacts, error) {
					return sessionctl.ResultFacts{Base: req.Commit, Head: strings.Repeat("cd", 20), Paths: []string{"go/store/a.go"}}, nil
				}
				resp = a.ObserveResult(context.Background(), relay.SessionRequest{Operation: relay.WorkspaceResultOperation, Arguments: resultArgs(t)})
			} else {
				resp = attestCall(a, attestArgs(nil))
			}
			if held || ensures != 1 || events[len(events)-1] != "process-start" || resp.OK != (kind != "persistent restoration") || resp.InboxLaunch != nil {
				t.Fatalf("ordinary relaunch changed: %+v (held: %t, ensures: %d, events: %v)", resp, held, ensures, events)
			}
		})
	}
}
