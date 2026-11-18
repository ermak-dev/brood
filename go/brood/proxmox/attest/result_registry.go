package attest

import "slices"

// IssueResult issues a bounded one-time result challenge with an exact pass binding.
// A new challenge replaces an older challenge for the same swarm/delivery attempt.
func (r *Registry) IssueResult(req Request, pass, sequence int64) (ResultRequest, error) {
	if pass <= 0 || sequence < pass {
		return ResultRequest{}, ErrMismatch
	}
	claim, err := r.Issue(req)
	if err != nil {
		return ResultRequest{}, err
	}
	result := ResultRequest{Claim: claim, Pass: pass, Sequence: sequence}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pending[claim.Challenge]
	if !ok || p.claim != claim {
		return ResultRequest{}, ErrReplay
	}
	p.result = &result
	r.pending[claim.Challenge] = p
	return result, nil
}

// ConsumeResult burns the challenge before verification, including failed verification.
// Keys derive from the issued identity, never actor-supplied identity fields.
func (r *Registry) ConsumeResult(got ResultAttestation, key func(string, int64) []byte) (ResultClaim, error) {
	p, err := r.takeChallenge(got.Challenge)
	if err != nil {
		return ResultClaim{}, err
	}
	if p.result == nil {
		return ResultClaim{}, ErrMismatch
	}
	if err := VerifyResult(key(p.claim.ActorUID, p.claim.Generation), *p.result, got); err != nil {
		return ResultClaim{}, err
	}
	result := got.ResultClaim
	result.Paths = slices.Clone(result.Paths)
	return result, nil
}

func (r *Registry) takeChallenge(challenge string) (pending, error) {
	r.mu.Lock()
	p, ok := r.pending[challenge]
	if ok {
		r.forget(challenge)
	}
	now := r.now()
	r.mu.Unlock()
	if !ok {
		return pending{}, ErrReplay
	}
	if !now.Before(p.expires) {
		return pending{}, ErrExpired
	}
	return p, nil
}
