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

	"brood.dev/go/brood/gitidentity"
	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

// checkoutHelperTimeout stays below the relay attestation budget, leaving room
// for the actor stop, relaunch and session start around it.
const checkoutHelperTimeout = 9 * time.Minute

var errStepSecretPublication = errors.New("project secrets could not be published; workspace not inspected")

// WorkspaceAttestor signs workspace attestations as the guest root. Token is the
// incarnation's relay token: it never reaches a helper or the response.
type WorkspaceAttestor struct {
	ActorUID   string
	Generation int64
	Token      string `json:"-"`
	// Origin is the root's own bootstrap origin; a request may only repeat it.
	Origin WorkspaceOrigin
	// Quiesce runs its callback while no actor process exists (Runtime.Quiesce).
	Quiesce func(context.Context, func(context.Context) error) error `json:"-"`
	// PrepareSecrets fetches an unpublished project projection. The returned
	// function publishes it only in the quiet window, after the old app stops.
	PrepareSecrets func(context.Context) (func() error, error) `json:"-"`
	// Prepare builds a pinned empty ephemeral workspace in the same quiet window.
	Prepare  func(context.Context, sessionctl.CheckoutRequest) error                             `json:"-"`
	Checkout func(context.Context, sessionctl.CheckoutRequest) (sessionctl.CheckoutFacts, error) `json:"-"`
	Observe  func(context.Context, sessionctl.CheckoutRequest) (sessionctl.ResultFacts, error)   `json:"-"`
	Ensure   func(context.Context) error                                                         `json:"-"`
	// Step puts the next Claude session into the step mode (#224) while no
	// actor process exists; Free returns it to the free mode. Both are root
	// effects of stepPolicy; restart and recycle call Free too.
	Step func(gitidentity.Identity) error `json:"-"`
	Free func() error                     `json:"-"`

	stepRuntime     *Runtime
	trackStepLaunch bool
	inboxReceiverID string
}

func (*WorkspaceAttestor) String() string     { return "guest.WorkspaceAttestor{redacted}" }
func (a *WorkspaceAttestor) GoString() string { return a.String() }

// Format serves every verb: String and GoString cover only some, and the rest
// would print the fields. %p bypasses Format and prints an address only.
func (a *WorkspaceAttestor) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, a.String()) }

// NewWorkspaceAttestor binds production effects to the root bootstrap identity
// and origin; quiesce is the runtime's Quiesce.
func NewWorkspaceAttestor(cfg relay.ClientConfig, origin WorkspaceOrigin, quiesce func(context.Context, func(context.Context) error) error) *WorkspaceAttestor {
	return newWorkspaceAttestor(cfg, origin, quiesce, systemStepPolicy())
}

// newWorkspaceAttestor is NewWorkspaceAttestor with the step policy paths that
// tests substitute.
func newWorkspaceAttestor(cfg relay.ClientConfig, origin WorkspaceOrigin, quiesce func(context.Context, func(context.Context) error) error, policy stepPolicy) *WorkspaceAttestor {
	return &WorkspaceAttestor{
		ActorUID: cfg.ActorUID, Generation: cfg.Generation, Token: cfg.Token, Origin: origin,
		Quiesce: quiesce, Prepare: runEphemeralPreparation, Checkout: runCheckoutHelper,
		Observe: runResultHelper, Ensure: ensureSessionAfterQuiesce,
		Step: policy.enter, Free: policy.leave,
	}
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
// while no actor process exists, puts the next session into the step mode,
// restores the session and only then signs. The claim is about that quiet
// window, not about later model actions; the step mode (#224) keeps the code
// the actor stored in its home from starting with the new session. Ephemeral
// actors are restricted to their bootstrap commit; refused preparation, checkout
// or step policy never starts a model session.
func (a *WorkspaceAttestor) Attest(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	return a.attest(ctx, ctx, req)
}

func (a *WorkspaceAttestor) attest(ctx, authority context.Context, req relay.SessionRequest) relay.SessionResponse {
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
	ephemeral := a.Origin.Ephemeral != nil
	if ephemeral {
		if !validPinnedCommit(a.Origin.Ephemeral.Commit) || a.Origin.Ephemeral.Commit != claim.Commit {
			return attestFailure("requested commit differs from the ephemeral bootstrap commit; nothing changed")
		}
		if a.Prepare == nil {
			return attestFailure("ephemeral workspace preparation is unavailable; nothing changed")
		}
	}
	if a.stepRuntime != nil {
		a.stepRuntime.stepOperation.Lock()
		defer a.stepRuntime.stepOperation.Unlock()
	}
	if ctx.Err() != nil || authority.Err() != nil {
		return attestFailure("workspace attestation canceled; nothing changed")
	}
	var publishSecrets func() error
	if a.PrepareSecrets != nil {
		prepared := refreshSessionSecrets(ctx, authority, func(ctx context.Context) error {
			var err error
			publishSecrets, err = a.PrepareSecrets(ctx)
			return err
		})
		if !prepared || publishSecrets == nil {
			return attestFailure("project secrets refresh failed; session unchanged")
		}
	}
	// После остановки актора эффекты доводятся до конца и без сервера: прерванный
	// checkout или актор без сессии хуже потерянного ответа. Их ограничивают
	// собственные бюджеты helper и runtime, а не запрос.
	work := context.WithoutCancel(ctx)
	var facts sessionctl.CheckoutFacts
	var checkoutErr, policyErr error
	apply := func(quiet context.Context) error {
		// The fetch has no actor-readable effects. Only the proven quiet window
		// may publish the bundle, and only while this request still authorizes it.
		if publishSecrets != nil && (ctx.Err() != nil || authority.Err() != nil || quiet.Err() != nil || publishSecrets() != nil) {
			return errStepSecretPublication
		}
		request := sessionctl.CheckoutRequest{Commit: claim.Commit, Origin: a.Origin.Origin, Mode: a.Origin.Mode, GitIdentity: a.Origin.GitIdentity}
		if ephemeral {
			bounded, cancel := context.WithTimeout(quiet, checkoutHelperTimeout)
			defer cancel()
			quiet = bounded
			checkoutErr = a.Prepare(quiet, request)
		}
		if checkoutErr == nil {
			facts, checkoutErr = a.Checkout(quiet, request)
		}
		// Ещё в окне тишины: rc-файлы актора не читает ни один процесс, а
		// сессия стартует уже в своём режиме. Отказ проверки — не шаг.
		if checkoutErr == nil && facts.Commit == claim.Commit {
			policyErr = a.Step(a.Origin.GitIdentity)
		} else if !ephemeral {
			policyErr = a.Free()
		}
		return checkoutErr
	}
	var quiesceErr error
	var step *stepLaunchRequest
	attested := false
	if a.stepRuntime == nil {
		quiesceErr = a.Quiesce(work, apply)
	} else {
		step = &stepLaunchRequest{claim: claim, request: ctx, authority: authority, inbox: a.inboxReceiverID != "", applied: func() bool {
			return a.trackStepLaunch && facts.Commit == claim.Commit && policyErr == nil
		}}
		quiesceErr = a.stepRuntime.transition(work, apply, step)
		defer func() { a.stepRuntime.finishStepLaunch(step.launch, ctx, attested) }()
	}
	if quiesceErr != nil && quiesceErr != checkoutErr {
		// Остановка не доказана или актор не перезапущен: runtime завершается.
		return attestFailure(quiesceErr.Error())
	}
	// Persistent восстанавливает сессию и после отказа; ephemeral не получает
	// автоматическую модель без успешно подготовленного и проверенного source.
	var sessionErr error
	if !ephemeral || checkoutErr == nil && facts.Commit == claim.Commit && policyErr == nil {
		sessionErr = a.Ensure(work)
	}
	refused := ""
	if (checkoutErr != nil || facts.Commit != claim.Commit) && policyErr != nil {
		// Свободной сессии не вышло: осталась политика прошлого шага.
		refused = "; the step session policy of the previous step was not cleared"
	}
	switch {
	case checkoutErr != nil && sessionErr != nil:
		return attestFailure(checkoutErr.Error() + refused + "; the Claude session did not start after the check")
	case checkoutErr != nil:
		return attestFailure(checkoutErr.Error() + refused)
	case facts.Commit != claim.Commit:
		return attestFailure("checkout reported another commit; no attestation" + refused)
	case policyErr != nil:
		return attestFailure("workspace is at the commit but the step session policy was not applied: " + policyErr.Error() + "; no attestation")
	case sessionErr != nil:
		return attestFailure("workspace is at the commit but the Claude session did not start; no attestation")
	}
	signature, err := attest.Sign(attest.Key(a.Token), claim)
	if err != nil {
		return attestFailure("attestation signing failed")
	}
	data, err := json.Marshal(attest.Attestation{Claim: claim, Signature: signature})
	if err != nil {
		return attestFailure("attestation encoding failed")
	}
	response := relay.SessionResponse{OK: true, Outcome: "complete", Text: string(data)}
	if a.inboxReceiverID != "" {
		if step == nil || step.launch == nil || step.launch.actorInbox.pid == 0 || step.launch.ctx.Err() != nil {
			return attestFailure("current inbox launch could not be proven")
		}
		binding := attest.InboxLaunchClaim{Claim: claim, LaunchID: step.launch.id, ReceiverID: a.inboxReceiverID}
		proof, err := attest.SignInboxLaunch(attest.InboxLaunchKey(a.Token), binding)
		if err != nil {
			return attestFailure("inbox launch proof could not be issued")
		}
		response.InboxLaunch = &attest.InboxLaunchProof{InboxLaunchClaim: binding, Signature: proof}
	}
	attested = true
	return response
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
