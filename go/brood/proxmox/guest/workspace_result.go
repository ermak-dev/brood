//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/guest/sessionctl"
	"brood.dev/go/brood/proxmox/relay"
)

type resultArguments struct {
	Swarm     string `json:"swarm"`
	Step      string `json:"step"`
	Origin    string `json:"origin"`
	Commit    string `json:"commit"`
	Challenge string `json:"challenge"`
	Pass      int64  `json:"pass"`
	Sequence  int64  `json:"sequence"`
}

func decodeResultArguments(raw []byte) (resultArguments, error) {
	fail := errors.New("invalid workspace result arguments")
	allowed := map[string]bool{"swarm": true, "step": true, "origin": true, "commit": true, "challenge": true, "pass": true, "sequence": true}
	seen := map[string]bool{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return resultArguments{}, fail
	}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return resultArguments{}, fail
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return resultArguments{}, fail
		}
	}
	if _, err := decoder.Token(); err != nil || len(seen) != len(allowed) {
		return resultArguments{}, fail
	}
	if decoder.Decode(new(any)) != io.EOF {
		return resultArguments{}, fail
	}
	var args resultArguments
	if json.Unmarshal(raw, &args) != nil {
		return resultArguments{}, fail
	}
	return args, nil
}

// ObserveResult signs a read-only committed result observed while no actor exists.
// Unlike Attest, this never checks out a commit or changes the step session policy.
func (a *WorkspaceAttestor) ObserveResult(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	args, err := decodeResultArguments(req.Arguments)
	if err != nil {
		return attestFailure("invalid workspace result request")
	}
	binding := attest.ResultRequest{Claim: attest.Claim{ActorUID: a.ActorUID, Generation: a.Generation, Swarm: args.Swarm, Step: args.Step, Origin: args.Origin, Commit: args.Commit, Challenge: args.Challenge}, Pass: args.Pass, Sequence: args.Sequence}
	if err := binding.Validate(); err != nil {
		return attestFailure(err.Error() + "; nothing changed")
	}
	if a.Origin.Origin == "" || a.Origin.Mode == "" || binding.Origin != a.Origin.Origin {
		return attestFailure("requested result origin differs from the actor workspace origin; nothing changed")
	}
	if a.Token == "" {
		return attestFailure("result attestation bootstrap credential is unavailable; nothing changed")
	}
	if a.Observe == nil || a.Quiesce == nil || a.Ensure == nil {
		return relay.SessionResponse{Text: "workspace result attestation is not supported by this guest", Outcome: "unsupported"}
	}
	if ctx.Err() != nil {
		return attestFailure("workspace result request canceled; nothing changed")
	}
	work := context.WithoutCancel(ctx)
	var facts sessionctl.ResultFacts
	var observationErr error
	quietErr := a.Quiesce(work, func(ctx context.Context) error {
		facts, observationErr = a.Observe(ctx, sessionctl.CheckoutRequest{Commit: binding.Commit, Origin: a.Origin.Origin, Mode: a.Origin.Mode, GitIdentity: a.Origin.GitIdentity})
		return observationErr
	})
	if quietErr != nil && !errors.Is(quietErr, observationErr) {
		return attestFailure("result observation did not complete a proven quiescent window")
	}
	sessionErr := a.Ensure(work)
	if sessionErr != nil {
		return attestFailure("Claude session did not start after result observation; no result attestation")
	}
	if observationErr != nil {
		return attestFailure(observationErr.Error() + "; no result attestation")
	}
	if facts.Base != binding.Commit {
		return attestFailure("result observer reported another input commit; no result attestation")
	}
	claim := attest.ResultClaim{ResultRequest: binding, Head: facts.Head, Paths: facts.Paths}
	signature, err := attest.SignResult(attest.ResultKey(a.Token), claim)
	if err != nil {
		return attestFailure("result observer returned incomplete or invalid evidence; no result attestation")
	}
	data, err := json.Marshal(attest.ResultAttestation{ResultClaim: claim, Signature: signature})
	if err != nil || len(data) > attest.MaxResultAttestationBytes {
		return attestFailure("result attestation exceeds size limit")
	}
	response := relay.SessionResponse{OK: true, Outcome: "complete", Text: string(data)}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded)+1 > relay.MaxSessionBytes {
		return attestFailure("result attestation exceeds relay response limit")
	}
	return response
}

func runResultHelper(ctx context.Context, request sessionctl.CheckoutRequest) (sessionctl.ResultFacts, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return sessionctl.ResultFacts{}, errors.New("result helper request encoding failed")
	}
	response := runHelper(ctx, relay.SessionRequest{Operation: sessionctl.ResultOperation, Arguments: data, Caller: "supervisor"}, checkoutHelperTimeout)
	if !response.OK || response.Outcome != "complete" {
		return sessionctl.ResultFacts{}, errors.New("result helper could not verify a clean committed workspace: " + response.Text)
	}
	var facts sessionctl.ResultFacts
	decoder := json.NewDecoder(bytes.NewReader([]byte(response.Text)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&facts) != nil || decoder.Decode(new(any)) != io.EOF {
		return sessionctl.ResultFacts{}, errors.New("invalid result helper response")
	}
	return facts, nil
}
