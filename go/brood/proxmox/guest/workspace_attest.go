//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

// checkoutHelperTimeout stays below the relay attestation budget, leaving room
// for the actor stop, relaunch and session start around it.
const checkoutHelperTimeout = 9 * time.Minute

// WorkspaceAttestor signs workspace attestations as the guest root. Token is the
// incarnation's relay token: it never reaches a helper or the response.
type WorkspaceAttestor struct {
	ActorUID   string
	Generation int64
	Token      string `json:"-"`
	// Origin is the root's own bootstrap origin; a request may only repeat it.
	Origin WorkspaceOrigin
	// Quiesce runs its callback while no actor process exists (Runtime.Quiesce).
	Quiesce  func(context.Context, func(context.Context) error) error                            `json:"-"`
	Checkout func(context.Context, sessionctl.CheckoutRequest) (sessionctl.CheckoutFacts, error) `json:"-"`
	Ensure   func(context.Context) error                                                         `json:"-"`
}

func (*WorkspaceAttestor) String() string     { return "guest.WorkspaceAttestor{redacted}" }
func (a *WorkspaceAttestor) GoString() string { return a.String() }

// Format serves every verb: String and GoString cover only some, and the rest
// would print the fields. %p bypasses Format and prints an address only.
func (a *WorkspaceAttestor) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, a.String()) }

// NewWorkspaceAttestor binds production effects to the root bootstrap identity
// and origin; quiesce is the runtime's Quiesce.
func NewWorkspaceAttestor(cfg relay.ClientConfig, origin WorkspaceOrigin, quiesce func(context.Context, func(context.Context) error) error) *WorkspaceAttestor {
	return &WorkspaceAttestor{ActorUID: cfg.ActorUID, Generation: cfg.Generation, Token: cfg.Token, Origin: origin, Quiesce: quiesce, Checkout: runCheckoutHelper, Ensure: ensureSessionAfterQuiesce}
}

type attestArguments struct {
	Swarm     string `json:"swarm"`
	Step      string `json:"step"`
	Origin    string `json:"origin"`
	Commit    string `json:"commit"`
	Challenge string `json:"challenge"`
}

func attestFailure(text string) relay.SessionResponse {
	return relay.SessionResponse{Text: text, Outcome: "failed"}
}

// Attest stops the whole actor, verifies the checkout of the requested commit
// while no actor process exists, restores the session and only then signs.
// The claim is about that quiet window, not about later model actions: code
// the actor keeps in its home runs with the new session and may still change
// the workspace after signing, which is open issue #224.
func (a *WorkspaceAttestor) Attest(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	args, err := decodeAttestArguments(req.Arguments)
	if err != nil {
		return attestFailure("invalid workspace attestation request; exactly swarm, step, origin, commit and challenge are accepted")
	}
	claim := attest.Claim{ActorUID: a.ActorUID, Generation: a.Generation, Swarm: args.Swarm, Step: args.Step, Origin: args.Origin, Commit: args.Commit, Challenge: args.Challenge}
	if err := claim.Validate(); err != nil {
		return attestFailure(err.Error() + "; nothing changed")
	}
	if a.Origin.Origin == "" || a.Origin.Mode == "" {
		return attestFailure("actor has no workspace origin; nothing changed")
	}
	if a.Origin.Origin != claim.Origin {
		return attestFailure("requested origin differs from the actor workspace origin; nothing changed")
	}
	if ctx.Err() != nil {
		return attestFailure("workspace attestation canceled; nothing changed")
	}
	// После остановки актора эффекты доводятся до конца и без сервера: прерванный
	// checkout или актор без сессии хуже потерянного ответа. Их ограничивают
	// собственные бюджеты helper и runtime, а не запрос.
	work := context.WithoutCancel(ctx)
	var facts sessionctl.CheckoutFacts
	var checkoutErr error
	quiesceErr := a.Quiesce(work, func(ctx context.Context) error {
		facts, checkoutErr = a.Checkout(ctx, sessionctl.CheckoutRequest{Commit: claim.Commit, Origin: a.Origin.Origin, Mode: a.Origin.Mode, GitIdentity: a.Origin.GitIdentity})
		return checkoutErr
	})
	if quiesceErr != nil && quiesceErr != checkoutErr {
		// Остановка не доказана или актор не перезапущен: runtime завершается.
		return attestFailure(quiesceErr.Error())
	}
	// Актор уже перезапущен: сессия нужна и после отказа проверки.
	sessionErr := a.Ensure(work)
	switch {
	case checkoutErr != nil && sessionErr != nil:
		return attestFailure(checkoutErr.Error() + "; the Claude session did not start after the check")
	case checkoutErr != nil:
		return attestFailure(checkoutErr.Error())
	case sessionErr != nil:
		return attestFailure("workspace is at the commit but the Claude session did not start; no attestation")
	case facts.Commit != claim.Commit:
		return attestFailure("checkout reported another commit; no attestation")
	}
	signature, err := attest.Sign(attest.Key(a.Token), claim)
	if err != nil {
		return attestFailure("attestation signing failed")
	}
	data, err := json.Marshal(attest.Attestation{Claim: claim, Signature: signature})
	if err != nil {
		return attestFailure("attestation encoding failed")
	}
	return relay.SessionResponse{OK: true, Outcome: "complete", Text: string(data)}
}

// decodeAttestArguments accepts one object with exactly the request fields;
// identity fields are not among them and are rejected as unknown.
func decodeAttestArguments(raw []byte) (attestArguments, error) {
	var args attestArguments
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 5 {
		return args, errors.New("arguments must be an object of five fields")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return args, errors.New("null argument")
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&args) != nil || d.Decode(new(any)) != io.EOF {
		return args, errors.New("invalid arguments")
	}
	return args, nil
}

func checkoutHelperRequest(r sessionctl.CheckoutRequest) (relay.SessionRequest, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return relay.SessionRequest{}, errors.New("checkout request encoding failed")
	}
	return relay.SessionRequest{Operation: sessionctl.CheckoutOperation, Arguments: data, Caller: "supervisor"}, nil
}

// checkoutFacts keeps the helper's refusal reason: during the quiet window the
// helper is the only actor process, so its text describes the real workspace.
func checkoutFacts(resp relay.SessionResponse) (sessionctl.CheckoutFacts, error) {
	var facts sessionctl.CheckoutFacts
	if !resp.OK || resp.Outcome != "complete" {
		if resp.Text == "" {
			return facts, errors.New("workspace checkout failed")
		}
		return facts, errors.New(resp.Text)
	}
	d := json.NewDecoder(bytes.NewReader([]byte(resp.Text)))
	d.DisallowUnknownFields()
	if d.Decode(&facts) != nil || d.Decode(new(any)) != io.EOF {
		return sessionctl.CheckoutFacts{}, errors.New("invalid checkout helper response")
	}
	return facts, nil
}

// runCheckoutHelper is the only path to the internal checkout operation.
func runCheckoutHelper(ctx context.Context, r sessionctl.CheckoutRequest) (sessionctl.CheckoutFacts, error) {
	req, err := checkoutHelperRequest(r)
	if err != nil {
		return sessionctl.CheckoutFacts{}, err
	}
	return checkoutFacts(runHelper(ctx, req, checkoutHelperTimeout))
}

// ensureSessionAfterQuiesce starts the fixed Claude session in the relaunched
// actor; the new PTY holder may take a moment to serve its socket.
func ensureSessionAfterQuiesce(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	for {
		err := launchAutostartHelper(ctx)
		if err == nil {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}
