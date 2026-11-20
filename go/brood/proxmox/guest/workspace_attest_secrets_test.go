//go:build linux

package guest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

// BUG: A new attested session can still have revoked project credentials.
// HYPOTHESIS: The connection handler refreshes before restart, but not before
// a verified workspace launch starts a new session.
// SOLUTION: Fetch after validation, publish only in the quiet window.
func TestWorkspaceAttestRefreshesProjectSecretsBeforeSessionReplacement(t *testing.T) {
	rec := &attestRecorder{}
	a := rec.attestor()
	current := "synthetic previous projection"
	ensure := a.Ensure
	a.Ensure = func(ctx context.Context) error {
		if current != "synthetic fresh projection" {
			t.Fatal("the attested app would start on the stale projection")
		}
		return ensure(ctx)
	}
	a.PrepareSecrets = func(context.Context) (func() error, error) {
		rec.events = append(rec.events, "refresh")
		return func() error {
			rec.events = append(rec.events, "publish")
			current = "synthetic fresh projection"
			return nil
		}, nil
	}
	handle := newSessionHandler(context.Background(), nil, a, func(context.Context, relay.SessionRequest) relay.SessionResponse {
		t.Fatal("the workspace request used the generic session helper")
		return relay.SessionResponse{}
	})
	response := handle(context.Background(), relay.SessionRequest{
		Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil), Caller: "server",
	})
	if !response.OK || response.Outcome != "complete" || strings.Contains(response.Text, current) {
		t.Fatalf("unexpected result of workspace attestation: outcome=%s ok=%t", response.Outcome, response.OK)
	}
	if want := []string{"refresh", "quiesce", "publish", "checkout", "step", "relaunch", "ensure"}; !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("session effects %v, want %v", rec.events, want)
	}
}

func TestWorkspaceAttestSecretRefreshRefusesWithoutSessionEffects(t *testing.T) {
	const secret = "synthetic private credential"
	for _, cause := range []string{"fetch failure", "connection canceled", "request canceled"} {
		t.Run(cause, func(t *testing.T) {
			rec := &attestRecorder{}
			connection, cancelConnection := context.WithCancel(context.Background())
			defer cancelConnection()
			request, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			a := rec.attestor()
			a.PrepareSecrets = func(bound context.Context) (func() error, error) {
				rec.events = append(rec.events, "refresh")
				switch cause {
				case "connection canceled":
					cancelConnection()
				case "request canceled":
					cancelRequest()
				default:
					return nil, errors.New(secret)
				}
				select {
				case <-bound.Done():
				case <-time.After(2 * time.Second):
					t.Fatal("refresh did not observe cancellation")
				}
				return func() error {
					t.Error("canceled preparation published a projection")
					return nil
				}, nil
			}
			handle := newSessionHandler(connection, nil, a, nil)
			response := handle(request, relay.SessionRequest{
				Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil), Caller: "server",
			})
			if response.OK || response.Outcome != "failed" || response.InboxLaunch != nil ||
				!strings.Contains(response.Text, "project secrets refresh failed") || strings.Contains(response.Text, secret) {
				t.Fatalf("refresh was not refused with a redacted error: outcome=%s ok=%t", response.Outcome, response.OK)
			}
			if want := []string{"refresh"}; !reflect.DeepEqual(rec.events, want) {
				t.Fatalf("session effects %v, want %v", rec.events, want)
			}
		})
	}
}

func TestWorkspaceAttestSecretPublishFailureDoesNotStartStep(t *testing.T) {
	const secret = "synthetic private projection"
	rec := &attestRecorder{}
	a := rec.attestor()
	a.PrepareSecrets = func(context.Context) (func() error, error) {
		rec.events = append(rec.events, "refresh")
		return func() error {
			rec.events = append(rec.events, "publish")
			return errors.New(secret)
		}, nil
	}
	response := attestCall(a, attestArgs(nil))
	if response.OK || response.InboxLaunch != nil ||
		!strings.Contains(response.Text, "project secrets could not be published") || strings.Contains(response.Text, secret) {
		t.Fatalf("publication was not refused safely: outcome=%s ok=%t", response.Outcome, response.OK)
	}
	if want := []string{"refresh", "quiesce", "publish", "relaunch"}; !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("failed publication changed the next step: effects %v, want %v", rec.events, want)
	}
}

func TestWorkspaceAttestPublishesPreparedSecretsOnlyWhenTheOldAppIsStopped(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	var old *fakeProcess
	var held *heldPTYTestProcess
	launch := c.Dependencies.Launch
	c.Dependencies.Launch = func(ctx context.Context, spec ActorSpec) (Process, error) {
		p, err := launch(ctx, spec)
		if old == nil {
			old = p.(*fakeProcess)
		}
		return p, err
	}
	withHeldPTYTestLauncher(c, func(p *heldPTYTestProcess) { held = p })
	r, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a := inboxPTYTestAttestor(r, &attestRecorder{})
	prepared, published := 0, 0
	a.PrepareSecrets = func(context.Context) (func() error, error) {
		prepared++
		select {
		case <-old.done:
			return nil, errors.New("prepared after stopping the current actor")
		default:
		}
		return func() error {
			select {
			case <-old.done:
			default:
				return errors.New("published while the old actor could still read the projection")
			}
			if held != nil {
				return errors.New("published after starting the next actor")
			}
			published++
			return nil
		}, nil
	}
	handle := NewSupervisorSessionHandler(context.Background(), nil, a)
	response := handle(context.Background(), relay.SessionRequest{
		Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil), Caller: "server",
	})
	if !response.OK || response.InboxLaunch == nil || prepared != 1 || published != 1 || held == nil || !held.serving {
		t.Fatalf("the held launch did not receive a quiet projection: ok=%t prepared=%d published=%d", response.OK, prepared, published)
	}
	previous := boundActorLaunch(r)
	a.PrepareSecrets = func(context.Context) (func() error, error) {
		return nil, errors.New("synthetic private credential")
	}
	response = handle(context.Background(), relay.SessionRequest{
		Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(map[string]any{"step": "request-2"}), Caller: "server",
	})
	if response.OK || response.InboxLaunch != nil || strings.Contains(response.Text, "synthetic private credential") || boundActorLaunch(r) != previous || previous == nil || previous.ctx.Err() != nil {
		t.Fatal("failed preparation retired or replaced the previous held launch")
	}
}

func TestWorkspaceAttestSecretHandoffRejectsRevokedRequestBeforeStoppingActor(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	r, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	before := append([]string(nil), events...)
	request, cancel := context.WithCancel(context.Background())
	cancel()
	checked := false
	err = r.transition(context.WithoutCancel(request), func(context.Context) error {
		checked = true
		return nil
	}, &stepLaunchRequest{request: request, authority: context.Background(), applied: func() bool { return false }})
	if err == nil || checked || !reflect.DeepEqual(events, before) {
		t.Fatalf("revoked handoff reached actor effects: error=%v checked=%t events=%v", err, checked, events)
	}
}

func TestWorkspaceAttestSecretHandoffCarriesAuthorityWithoutBindings(t *testing.T) {
	r := &Runtime{done: make(chan struct{}), stop: make(chan struct{}), quiesce: make(chan quiesceRequest)}
	defer close(r.done)
	request, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	connection, cancelConnection := context.WithCancel(context.Background())
	defer cancelConnection()
	a := r.NewWorkspaceAttestor(stepLaunchTestConfig, WorkspaceOrigin{Origin: attestOrigin, Mode: "gateway"})
	handle := NewSupervisorSessionHandler(connection, nil, a)
	result := make(chan relay.SessionResponse, 1)
	go func() {
		result <- handle(request, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil)})
	}()
	select {
	case q := <-r.quiesce:
		if q.step == nil || q.step.request != request || q.step.authority != connection || q.step.inbox {
			t.Error("ordinary workspace handoff lost the request/connection fence or enabled inbox transport")
		}
		cancelRequest()
		q.reply <- errors.New("workspace attestation canceled before quiesce; session unchanged")
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary workspace request did not reach the runtime handoff")
	}
	select {
	case response := <-result:
		if response.OK || response.InboxLaunch != nil {
			t.Error("revoked non-binding workspace request returned a proof")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary workspace refusal did not finish")
	}
}

func TestWorkspaceAttestSecretRefreshKeepsOrdinaryLaunchUnbound(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	r, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a := r.NewWorkspaceAttestor(stepLaunchTestConfig, WorkspaceOrigin{Origin: attestOrigin, Mode: "gateway"})
	rec := &attestRecorder{}
	effects := rec.attestor()
	a.Checkout, a.Step, a.Ensure = effects.Checkout, effects.Step, effects.Ensure
	a.PrepareSecrets = func(context.Context) (func() error, error) {
		return func() error { return nil }, nil
	}
	response := attestCall(a, attestArgs(nil))
	if !response.OK || response.InboxLaunch != nil || boundActorLaunch(r) != nil {
		t.Fatal("ordinary attestation failed or enabled a launch binding")
	}
}

func TestWorkspaceAttestSecretPublicationFailureEndsOrdinaryRuntime(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	r, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a := r.NewWorkspaceAttestor(stepLaunchTestConfig, WorkspaceOrigin{Origin: attestOrigin, Mode: "gateway"})
	a.PrepareSecrets = func(context.Context) (func() error, error) {
		return func() error { return errors.New("synthetic private partial publication") }, nil
	}
	rec := &attestRecorder{}
	effects := rec.attestor()
	a.Checkout, a.Step, a.Ensure = effects.Checkout, effects.Step, effects.Ensure
	response := attestCall(a, attestArgs(nil))
	if response.OK || response.InboxLaunch != nil || len(rec.events) != 0 || strings.Contains(response.Text, "synthetic private") {
		t.Fatalf("failed projection reached a new step or leaked a cause: ok=%t effects=%v", response.OK, rec.events)
	}
	select {
	case <-r.done:
		if r.Wait() == nil || strings.Count(strings.Join(events, " "), "process-start") != 1 {
			t.Fatalf("publication failure was not fatal before a new app: %v", events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("publication failure left the stopped session backed by an idle holder")
	}
}

func TestWorkspaceAttestSecretRefreshSkipsUnverifiedRequests(t *testing.T) {
	for _, fields := range []map[string]any{{"step": nil}, {"origin": "https://other.example.org/group/project"}} {
		rec := &attestRecorder{}
		a := rec.attestor()
		a.PrepareSecrets = func(context.Context) (func() error, error) {
			t.Fatal("an unverified request triggered a secret refresh")
			return nil, nil
		}
		handle := newSessionHandler(context.Background(), nil, a, nil)
		response := handle(context.Background(), relay.SessionRequest{
			Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(fields), Caller: "server",
		})
		if response.OK || response.Outcome != "failed" || response.InboxLaunch != nil || len(rec.events) != 0 {
			t.Fatalf("invalid request changed the session: outcome=%s ok=%t effects=%v", response.Outcome, response.OK, rec.events)
		}
	}
}
