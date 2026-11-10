package attest

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// MaxAttestationBytes bounds an encoded guest attestation.
const MaxAttestationBytes = 4096

var (
	// ErrReplay: the challenge was never issued, is used up or was replaced.
	ErrReplay = errors.New("attestation challenge is unknown, already used or replaced")
	// ErrExpired: the challenge outlived its validity.
	ErrExpired = errors.New("attestation challenge expired")
	// ErrIncarnation: another incarnation of the actor answered.
	ErrIncarnation = errors.New("attestation is from another actor incarnation")
	// ErrMismatch: the claim names another actor, attempt, source or challenge.
	ErrMismatch = errors.New("attestation claim differs from the request")
	// ErrSignature: the claim is not signed by the expected incarnation.
	ErrSignature = errors.New("attestation signature is invalid")
)

// Verify checks a guest attestation against the claim the server issued.
// The key must be derived from want's identity, never from got's.
func Verify(key []byte, want Claim, got Attestation) error {
	if len(key) == 0 {
		return errors.New("attestation key is required")
	}
	if err := want.Validate(); err != nil {
		return err
	}
	switch {
	case got.ActorUID != want.ActorUID:
		return fmt.Errorf("%w: actor", ErrMismatch)
	case got.Generation != want.Generation:
		return ErrIncarnation
	case got.Swarm != want.Swarm || got.Step != want.Step:
		return fmt.Errorf("%w: step attempt", ErrMismatch)
	case got.Origin != want.Origin:
		return fmt.Errorf("%w: origin", ErrMismatch)
	case got.Commit != want.Commit:
		return fmt.Errorf("%w: commit", ErrMismatch)
	case got.Challenge != want.Challenge:
		return fmt.Errorf("%w: challenge", ErrMismatch)
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(got.Signature)
	if err != nil || len(sig) != sha256.Size || !hmac.Equal(sig, mac(key, want)) {
		return ErrSignature
	}
	return nil
}

// DecodeAttestation parses exactly one bounded JSON object with the guest's
// fields; duplicates, nulls, unknown fields and trailing data are refused.
func DecodeAttestation(data []byte) (Attestation, error) {
	var a Attestation
	fail := errors.New("malformed workspace attestation")
	if len(data) > MaxAttestationBytes || !uniqueFields(data) {
		return a, fail
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&a) != nil || d.Decode(new(any)) != io.EOF {
		return Attestation{}, fail
	}
	return a, nil
}

func uniqueFields(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	if open, err := d.Token(); err != nil || open != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return false
		}
	}
	_, err := d.Token()
	return err == nil
}

// Request is the part of a claim the server fixes before asking the guest.
type Request struct {
	ActorUID   string
	Generation int64
	Swarm      string
	Step       string
	Origin     string
	Commit     string
}

type pending struct {
	claim   Claim
	expires time.Time
}

type attempt struct{ swarm, step string }

// Registry issues one-time challenges and consumes each at most once.
//
// Состояние живёт в памяти сервера: перезапуск забывает выданные challenge,
// и все прежние подтверждения отвергаются — fail-closed без схемы SQLite.
// База одна и под Recreate, второго экземпляра, которому нужна общая
// таблица, нет. Потребитель (#212) вызывает Consume перед транзакцией
// inflight, а транзакция заново проверяет инкарнацию и привязку роли.
type Registry struct {
	mu       sync.Mutex
	pending  map[string]pending
	attempts map[attempt]string
	ttl      time.Duration
	limit    int
	now      func() time.Time
	random   io.Reader
}

// NewRegistry keeps at most limit unexpired challenges, each valid for ttl.
func NewRegistry(ttl time.Duration, limit int) *Registry {
	return &Registry{pending: map[string]pending{}, attempts: map[attempt]string{}, ttl: ttl, limit: limit, now: time.Now, random: rand.Reader}
}

// Issue returns the claim the guest must sign, with a fresh challenge. A new
// challenge for the same step attempt replaces the previous one.
func (r *Registry) Issue(req Request) (Claim, error) {
	raw := make([]byte, ChallengeBytes)
	if _, err := io.ReadFull(r.random, raw); err != nil {
		return Claim{}, errors.New("attestation challenge randomness unavailable")
	}
	claim := Claim{ActorUID: req.ActorUID, Generation: req.Generation, Swarm: req.Swarm, Step: req.Step, Origin: req.Origin, Commit: req.Commit, Challenge: base64.RawURLEncoding.EncodeToString(raw)}
	if err := claim.Validate(); err != nil {
		return Claim{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for challenge, p := range r.pending {
		if !now.Before(p.expires) {
			r.forget(challenge)
		}
	}
	key := attempt{claim.Swarm, claim.Step}
	if old, ok := r.attempts[key]; ok {
		r.forget(old)
	}
	if len(r.pending) >= r.limit {
		return Claim{}, errors.New("too many pending workspace attestations")
	}
	r.pending[claim.Challenge] = pending{claim: claim, expires: now.Add(r.ttl)}
	r.attempts[key] = claim.Challenge
	return claim, nil
}

// Consume burns the answered challenge before checking it, so a replay, a
// forgery or a late answer can never be retried against the same challenge.
// key derives the incarnation key from the issued actor UID and generation.
func (r *Registry) Consume(got Attestation, key func(actorUID string, generation int64) []byte) (Claim, error) {
	r.mu.Lock()
	p, ok := r.pending[got.Challenge]
	if ok {
		r.forget(got.Challenge)
	}
	now := r.now()
	r.mu.Unlock()
	if !ok {
		return Claim{}, ErrReplay
	}
	if !now.Before(p.expires) {
		return Claim{}, ErrExpired
	}
	if err := Verify(key(p.claim.ActorUID, p.claim.Generation), p.claim, got); err != nil {
		return Claim{}, err
	}
	return p.claim, nil
}

func (r *Registry) forget(challenge string) {
	if p, ok := r.pending[challenge]; ok {
		delete(r.pending, challenge)
		key := attempt{p.claim.Swarm, p.claim.Step}
		if r.attempts[key] == challenge {
			delete(r.attempts, key)
		}
	}
}
