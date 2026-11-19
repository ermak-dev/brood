//go:build linux

package guest

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/guest/managedinbox"
	"brood.dev/go/brood/proxmox/relay"
)

// FEATURE: Apply managed inbox settings only to the one held step launch.
// SPEC: Restore normal settings before any ordinary relaunch, including a
// connection cancellation between a held spawn and its release.
func TestStepInboxPolicyFollowsHeldLaunchAndNeverOrdinaryHolder(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "ordinary relaunch"
		if revoke {
			name = "canceled launch fallback"
		}
		t.Run(name, func(t *testing.T) {
			var events []string
			c, _ := testConfig(&events)
			connection, disconnect := context.WithCancel(context.Background())
			defer disconnect()
			active := false
			var writes, restores, heldStarts, ordinaryStarts int
			launch := c.Dependencies.Launch
			c.Dependencies.Launch = func(ctx context.Context, spec ActorSpec) (Process, error) {
				if spec.ptyStartHeld() {
					heldStarts++
					if !active {
						t.Error("held launch started before its managed inbox policy")
					}
				} else {
					ordinaryStarts++
					if active {
						t.Error("an ordinary holder inherited an inbox managed policy")
					}
				}
				return launch(ctx, spec)
			}
			withHeldPTYTestLauncher(c, func(p *heldPTYTestProcess) {
				if revoke {
					p.identify = func() error { disconnect(); return nil }
				}
			})
			r, err := Start(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			r.stepMu.Lock()
			r.inboxPolicy = &stepInboxSessionPolicy{
				enter: func() error { active = true; writes++; return nil },
				leave: func() error { active = false; restores++; return nil },
			}
			r.stepMu.Unlock()
			a := inboxPTYTestAttestor(r, &attestRecorder{})
			a.Ensure = func(context.Context) error {
				if revoke || !active {
					t.Error("initial app ensure escaped its exact managed inbox launch")
				}
				return nil
			}
			resp := a.attest(context.Background(), connection, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil)})
			if resp.OK == revoke || (resp.InboxLaunch != nil) == revoke {
				t.Fatalf("wrong outcome for held launch: %+v", resp)
			}
			if !revoke {
				if err := r.Quiesce(context.Background(), func(context.Context) error {
					if active {
						t.Error("ordinary quiescence saw the retired inbox override")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if active || writes != 1 || restores != 1 || heldStarts != 1 || ordinaryStarts != 2 {
				t.Fatalf("scoped activation was not applied and retired exactly once: active=%t writes=%d restores=%d held=%d ordinary=%d", active, writes, restores, heldStarts, ordinaryStarts)
			}
		})
	}
}

func TestStepInboxDisconnectAfterAttestationRestoresOrdinaryHolder(t *testing.T) {
	p := stepTestPolicy(t)
	layout := stepTestLayout(t, p)
	bundle, ok := managedinbox.Package(true)
	if !ok {
		t.Fatal("managed inbox package unavailable")
	}

	var events []string
	c, _ := testConfig(&events)
	type holderStart struct {
		process Process
		managed string
	}
	starts := make(chan holderStart, 1)
	withHeldPTYTestLauncher(c, func(*heldPTYTestProcess) {})
	launch := c.Dependencies.Launch
	var previous Process
	c.Dependencies.Launch = func(ctx context.Context, spec ActorSpec) (Process, error) {
		if held, ok := previous.(*heldPTYTestProcess); ok {
			select {
			case <-held.done:
			default:
				t.Error("replacement launched before the held process stopped")
			}
		}
		next, err := launch(ctx, spec)
		if err == nil {
			previous = next
			managed, err := os.ReadFile(p.dropIn())
			if err != nil && !os.IsNotExist(err) {
				t.Errorf("read managed policy at holder start: %v", err)
			}
			select {
			case starts <- holderStart{next, string(managed)}:
			default:
				t.Error("unexpected extra holder start")
			}
		}
		return next, err
	}
	r, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	nextStart := func(why string) holderStart {
		t.Helper()
		select {
		case start := <-starts:
			return start
		case <-r.done:
			t.Fatalf("guest stopped while waiting for %s: %v", why, r.Wait())
		case <-time.After(5 * time.Second):
			t.Fatalf("no holder started for %s", why)
		}
		return holderStart{}
	}
	nextStart("initial guest startup")
	r.stepMu.Lock()
	r.inboxPolicy = &stepInboxSessionPolicy{
		enter: func() error { return p.writeManagedInbox(bundle.ManagedSettings) },
		leave: p.writeManaged,
	}
	r.stepMu.Unlock()

	a := inboxPTYTestAttestor(r, &attestRecorder{})
	a.Step, a.Free = p.enter, p.leave
	var ensures atomic.Int32
	a.Ensure = func(context.Context) error {
		ensures.Add(1)
		_, env, err := prepareInboxClaudeLaunch(layout, io.Discard)
		if err != nil {
			return err
		}
		if !slices.Contains(env, "CLAUDE_CODE_PLUGIN_CACHE_DIR="+managedinbox.PluginStateDirectory) {
			return errors.New("held app did not use the protected inbox cache")
		}
		return nil
	}
	attestStep := func(connection context.Context, step string) *stepLaunch {
		t.Helper()
		request, cancel := context.WithCancel(connection)
		handle := NewSupervisorSessionHandler(connection, nil, a)
		resp := handle(request, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(map[string]any{"step": step})})
		cancel() // The successful RPC ends before the relay connection does.
		binding := boundActorLaunch(r)
		if !resp.OK || resp.InboxLaunch == nil || binding == nil || resp.InboxLaunch.LaunchID != binding.id {
			t.Fatalf("held attestation did not establish a live binding: %+v", resp)
		}
		return binding
	}

	connection, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	first := attestStep(connection, "request-1")
	start := nextStart("the first held attestation")
	held, ok := start.process.(*heldPTYTestProcess)
	if !ok || !held.serving || held.releaseCalls != 1 || !strings.Contains(start.managed, `"enabledPlugins"`) || ensures.Load() != 1 {
		t.Fatal("attestation did not start exactly one released app with inbox policy")
	}

	// No quiesce or ensure from the test may trigger recovery.
	disconnect()
	restored := nextStart("disconnect recovery")
	if _, held := restored.process.(*heldPTYTestProcess); held || restored.managed != string(stepManagedDocument()) {
		t.Fatal("disconnect did not restore an ordinary holder with the baseline managed policy")
	}
	if first.ctx.Err() == nil || boundActorLaunch(r) != nil || ensures.Load() != 1 {
		t.Fatal("disconnect left inbox authority or automatically ensured another app")
	}

	reconnected, closeSuccessor := context.WithCancel(context.Background())
	defer closeSuccessor()
	successor := attestStep(reconnected, "request-2")
	start = nextStart("the successor held attestation")
	nextHeld, ok := start.process.(*heldPTYTestProcess)
	if !ok || nextHeld == held || !nextHeld.serving || successor.id == first.id || ensures.Load() != 2 {
		t.Fatal("a fresh connection did not establish its own held app")
	}
	select {
	case <-nextHeld.done:
		t.Fatal("disconnect recovery stopped the successor holder")
	default:
	}
	if boundActorLaunch(r) != successor {
		t.Fatal("disconnect recovery revoked the successor binding")
	}
}
