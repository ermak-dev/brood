//go:build linux

package guest

// FEATURE: Root-signed workspace attestation (#183).
// SPEC: Sign only after a quiet checkout and a restored session; identity comes
// from the root bootstrap, not the request; any refusal carries no signature;
// the internal checkout operation is unreachable from the relay.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

const attestToken = "root-only-relay-token-0123456789abcdefghijk"

var (
	attestCommit    = strings.Repeat("ab", 20)
	attestChallenge = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	attestOrigin    = "https://git.example.com/group/project"
)

type attestRecorder struct {
	events   []string
	checkout sessionctl.CheckoutRequest
	fail     map[string]error
	facts    *sessionctl.CheckoutFacts
}

func (r *attestRecorder) attestor() *WorkspaceAttestor {
	return &WorkspaceAttestor{ActorUID: "actor-1", Generation: 7, Token: attestToken, Origin: WorkspaceOrigin{Origin: attestOrigin, Mode: "gateway"},
		Quiesce: func(ctx context.Context, fn func(context.Context) error) error {
			r.events = append(r.events, "quiesce")
			if err := r.fail["quiesce"]; err != nil {
				return err
			}
			err := fn(ctx)
			r.events = append(r.events, "relaunch")
			return err
		},
		Checkout: func(_ context.Context, req sessionctl.CheckoutRequest) (sessionctl.CheckoutFacts, error) {
			r.events = append(r.events, "checkout")
			r.checkout = req
			if r.facts != nil {
				return *r.facts, nil
			}
			return sessionctl.CheckoutFacts{Commit: req.Commit}, r.fail["checkout"]
		},
		Ensure: func(context.Context) error {
			r.events = append(r.events, "ensure")
			return r.fail["ensure"]
		}}
}

func attestArgs(fields map[string]any) json.RawMessage {
	args := map[string]any{"swarm": "01JSWARM0000000000000000AA", "step": "request-1", "origin": attestOrigin, "commit": attestCommit, "challenge": attestChallenge}
	for k, v := range fields {
		if v == nil {
			delete(args, k)
		} else {
			args[k] = v
		}
	}
	data, _ := json.Marshal(args)
	return data
}

func attestCall(a *WorkspaceAttestor, args json.RawMessage) relay.SessionResponse {
	return a.Attest(context.Background(), relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: args, Caller: "server"})
}

func TestWorkspaceAttestSignsAfterQuietCheckoutAndSession(t *testing.T) {
	rec := &attestRecorder{}
	r := attestCall(rec.attestor(), attestArgs(nil))
	if !r.OK || r.Outcome != "complete" {
		t.Fatalf("%+v", r)
	}
	if want := []string{"quiesce", "checkout", "relaunch", "ensure"}; !reflect.DeepEqual(rec.events, want) {
		t.Fatalf("events %v", rec.events)
	}
	if rec.checkout != (sessionctl.CheckoutRequest{Commit: attestCommit, Origin: attestOrigin, Mode: "gateway"}) {
		t.Fatalf("checkout request %+v", rec.checkout)
	}
	if strings.Contains(r.Text, attestToken) {
		t.Fatal("relay token in the response")
	}
	var got attest.Attestation
	d := json.NewDecoder(strings.NewReader(r.Text))
	d.DisallowUnknownFields()
	if err := d.Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := attest.Claim{ActorUID: "actor-1", Generation: 7, Swarm: "01JSWARM0000000000000000AA", Step: "request-1", Origin: attestOrigin, Commit: attestCommit, Challenge: attestChallenge}
	if got.Claim != want {
		t.Fatalf("claim %+v", got.Claim)
	}
	if sig, _ := attest.Sign(attest.Key(attestToken), want); got.Signature != sig {
		t.Fatal("signature is not the root token signature of the claim")
	}
}

func TestWorkspaceAttestRefusesBeforeAnyEffect(t *testing.T) {
	for name, args := range map[string]json.RawMessage{
		"identity injection": attestArgs(map[string]any{"actor_uid": "actor-2"}),
		"generation":         attestArgs(map[string]any{"generation": 8}),
		"unknown field":      attestArgs(map[string]any{"force": true}),
		"missing challenge":  attestArgs(map[string]any{"challenge": nil}),
		"short challenge":    attestArgs(map[string]any{"challenge": "abc"}),
		"short commit":       attestArgs(map[string]any{"commit": "abc"}),
		"other origin":       attestArgs(map[string]any{"origin": "https://git.example.com/group/other"}),
		"bad step":           attestArgs(map[string]any{"step": "a b"}),
		"not an object":      json.RawMessage(`[]`),
		"null field":         json.RawMessage(`{"swarm":null}`),
	} {
		t.Run(name, func(t *testing.T) {
			rec := &attestRecorder{}
			r := attestCall(rec.attestor(), args)
			if r.OK || len(rec.events) != 0 || strings.Contains(r.Text, "signature") {
				t.Fatalf("%+v events %v", r, rec.events)
			}
		})
	}
	rec := &attestRecorder{}
	a := rec.attestor()
	a.Origin = WorkspaceOrigin{}
	if r := attestCall(a, attestArgs(nil)); r.OK || len(rec.events) != 0 {
		t.Fatalf("attested an actor without origin: %+v", r)
	}
}

func TestWorkspaceAttestRefusalsCarryNoSignature(t *testing.T) {
	other := sessionctl.CheckoutFacts{Commit: strings.Repeat("cd", 20)}
	for name, c := range map[string]struct {
		rec    *attestRecorder
		reason string
		events []string
	}{
		"dirty":            {&attestRecorder{fail: map[string]error{"checkout": errors.New("workspace has local changes (1 untracked); commit or remove them, nothing changed")}}, "1 untracked", []string{"quiesce", "checkout", "relaunch", "ensure"}},
		"unproven stop":    {&attestRecorder{fail: map[string]error{"quiesce": errors.New("actor processes could not be proven stopped")}}, "proven stopped", []string{"quiesce"}},
		"session":          {&attestRecorder{fail: map[string]error{"ensure": errors.New("no session")}}, "session", []string{"quiesce", "checkout", "relaunch", "ensure"}},
		"other commit":     {&attestRecorder{facts: &other}, "commit", []string{"quiesce", "checkout", "relaunch", "ensure"}},
		"dirty no session": {&attestRecorder{fail: map[string]error{"checkout": errors.New("workspace has local changes"), "ensure": errors.New("no session")}}, "session did not start", []string{"quiesce", "checkout", "relaunch", "ensure"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := attestCall(c.rec.attestor(), attestArgs(nil))
			if r.OK || r.Outcome != "failed" || strings.Contains(r.Text, "signature") || !strings.Contains(r.Text, c.reason) {
				t.Fatalf("%+v", r)
			}
			if !reflect.DeepEqual(c.rec.events, c.events) {
				t.Fatalf("events %v", c.rec.events)
			}
		})
	}
}

func TestSessionHandlerKeepsCheckoutInternal(t *testing.T) {
	ran := 0
	run := func(context.Context, relay.SessionRequest) relay.SessionResponse {
		ran++
		return relay.SessionResponse{OK: true, Outcome: "complete", Text: "{}"}
	}
	rec := &attestRecorder{}
	handler := newSessionHandler(context.Background(), nil, rec.attestor(), run)
	if r := handler(context.Background(), relay.SessionRequest{Operation: sessionctl.CheckoutOperation, Arguments: json.RawMessage(`{}`), Caller: "server"}); r.OK || ran != 0 {
		t.Fatalf("relay reached the internal checkout: %+v", r)
	}
	if r := handler(context.Background(), relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil), Caller: "server"}); !r.OK || ran != 0 || len(rec.events) == 0 {
		t.Fatalf("attestation not routed to the root attestor: %+v", r)
	}
	unsupported := newSessionHandler(context.Background(), nil, nil, run)
	if r := unsupported(context.Background(), relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil), Caller: "server"}); r.OK || r.Outcome != "unsupported" || ran != 0 {
		t.Fatalf("attestation without root attestor: %+v", r)
	}
	if r := runSessionHelper(context.Background(), relay.SessionRequest{Operation: sessionctl.CheckoutOperation, Arguments: json.RawMessage(`{}`), Caller: "server"}); r.OK || !strings.Contains(r.Text, "internal") {
		t.Fatalf("session helper path accepted the internal checkout: %+v", r)
	}
}

func TestCheckoutHelperRequest(t *testing.T) {
	req, err := checkoutHelperRequest(sessionctl.CheckoutRequest{Commit: attestCommit, Origin: attestOrigin, Mode: "direct"})
	if err != nil || req.Operation != sessionctl.CheckoutOperation || req.Caller != "supervisor" {
		t.Fatal(req, err)
	}
	var back sessionctl.CheckoutRequest
	if json.Unmarshal(req.Arguments, &back) != nil || back.Mode != "direct" || back.Commit != attestCommit {
		t.Fatalf("%s", req.Arguments)
	}
	facts, err := checkoutFacts(relay.SessionResponse{OK: true, Outcome: "complete", Text: `{"commit":"` + attestCommit + `","fetched":true}`})
	if err != nil || facts != (sessionctl.CheckoutFacts{Commit: attestCommit, Fetched: true}) {
		t.Fatal(facts, err)
	}
	for _, r := range []relay.SessionResponse{
		{Outcome: "failed", Text: "workspace has local changes"},
		{OK: true, Outcome: "complete", Text: `{"commit":"` + attestCommit + `","extra":1}`},
		{OK: true, Outcome: "complete", Text: `not json`},
	} {
		if _, err := checkoutFacts(r); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	if _, err := checkoutFacts(relay.SessionResponse{Outcome: "failed", Text: "workspace has local changes"}); err == nil || err.Error() != "workspace has local changes" {
		t.Fatalf("helper reason lost: %v", err)
	}
}

// A lost server connection must not abort a started checkout or leave the
// relaunched actor without its Claude session.
func TestWorkspaceAttestFinishesEffectsAfterDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &attestRecorder{}
	a := rec.attestor()
	quiesce, checkout, ensure := a.Quiesce, a.Checkout, a.Ensure
	var canceled []string
	a.Quiesce = func(ctx context.Context, fn func(context.Context) error) error {
		cancel()
		return quiesce(ctx, fn)
	}
	a.Checkout = func(ctx context.Context, r sessionctl.CheckoutRequest) (sessionctl.CheckoutFacts, error) {
		if ctx.Err() != nil {
			canceled = append(canceled, "checkout")
		}
		return checkout(ctx, r)
	}
	a.Ensure = func(ctx context.Context) error {
		if ctx.Err() != nil {
			canceled = append(canceled, "ensure")
		}
		return ensure(ctx)
	}
	a.Attest(ctx, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil), Caller: "server"})
	if len(canceled) != 0 || len(rec.events) != 4 {
		t.Fatalf("effects canceled by the request: %v, events %v", canceled, rec.events)
	}
	rec = &attestRecorder{}
	if r := rec.attestor().Attest(ctx, relay.SessionRequest{Operation: relay.WorkspaceAttestOperation, Arguments: attestArgs(nil), Caller: "server"}); r.OK || len(rec.events) != 0 {
		t.Fatalf("canceled request started effects: %+v %v", r, rec.events)
	}
}
