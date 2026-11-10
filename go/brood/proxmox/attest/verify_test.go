package attest

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func signed(t *testing.T, token string, c Claim) Attestation {
	t.Helper()
	sig, err := Sign(Key(token), c)
	if err != nil {
		t.Fatal(err)
	}
	return Attestation{Claim: c, Signature: sig}
}

func TestVerifyAcceptsOnlyTheExpectedSignedClaim(t *testing.T) {
	want := validClaim()
	if err := Verify(Key(testToken), want, signed(t, testToken, want)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		change func(*Attestation)
		is     error
	}{
		"other incarnation": {func(a *Attestation) { a.Generation = 8 }, ErrIncarnation},
		"other actor":       {func(a *Attestation) { a.ActorUID = "actor-2" }, ErrMismatch},
		"other swarm":       {func(a *Attestation) { a.Swarm = "01JSWARM0000000000000000AB" }, ErrMismatch},
		"other attempt":     {func(a *Attestation) { a.Step = "request-2" }, ErrMismatch},
		"other origin":      {func(a *Attestation) { a.Origin = "https://git.example.com/group/other" }, ErrMismatch},
		"other commit":      {func(a *Attestation) { a.Commit = strings.Repeat("cd", 20) }, ErrMismatch},
		"other challenge":   {func(a *Attestation) { a.Challenge = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)) }, ErrMismatch},
		"tampered":          {func(a *Attestation) { a.Signature = strings.Repeat("A", 43) }, ErrSignature},
		"padded":            {func(a *Attestation) { a.Signature += "=" }, ErrSignature},
		"empty":             {func(a *Attestation) { a.Signature = "" }, ErrSignature},
	} {
		t.Run(name, func(t *testing.T) {
			got := signed(t, testToken, want)
			c.change(&got)
			if err := Verify(Key(testToken), want, got); !errors.Is(err, c.is) {
				t.Fatalf("got %v, want %v", err, c.is)
			}
		})
	}
	// Подпись другой инкарнации над её собственным claim всё равно чужая.
	other := want
	other.Generation = 8
	if err := Verify(Key(testToken), want, signed(t, testToken+"8", other)); !errors.Is(err, ErrIncarnation) {
		t.Fatal(err)
	}
	if err := Verify(Key(testToken+"8"), want, signed(t, testToken, want)); !errors.Is(err, ErrSignature) {
		t.Fatal("verified with the key of another incarnation")
	}
	if err := Verify(nil, want, signed(t, testToken, want)); err == nil {
		t.Fatal("verified without a key")
	}
}

func TestDecodeAttestationIsStrict(t *testing.T) {
	a := signed(t, testToken, validClaim())
	data := []byte(`{"actor_uid":"actor-1","generation":7,"swarm":"` + a.Swarm + `","step":"request-1","origin":"` + a.Origin + `","commit":"` + a.Commit + `","challenge":"` + a.Challenge + `","signature":"` + a.Signature + `"}`)
	got, err := DecodeAttestation(data)
	if err != nil || got != a {
		t.Fatalf("%+v %v", got, err)
	}
	for name, bad := range map[string][]byte{
		"unknown field": bytes.Replace(data, []byte(`{`), []byte(`{"extra":1,`), 1),
		"duplicate key": bytes.Replace(data, []byte(`{`), []byte(`{"generation":8,`), 1),
		"trailing":      append(append([]byte{}, data...), []byte(` {}`)...),
		"not an object": []byte(`[]`),
		"null field":    bytes.Replace(data, []byte(`"generation":7`), []byte(`"generation":null`), 1),
		"oversize":      append(bytes.Repeat([]byte(" "), MaxAttestationBytes), data...),
	} {
		if _, err := DecodeAttestation(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newTestRegistry(c *clock, limit int) *Registry {
	r := NewRegistry(15*time.Minute, limit)
	r.now = c.Now
	return r
}

func testRequest(step string) Request {
	c := validClaim()
	return Request{ActorUID: c.ActorUID, Generation: c.Generation, Swarm: c.Swarm, Step: step, Origin: c.Origin, Commit: c.Commit}
}

// keyFor stands for the server's derivation from its relay secret.
func keyFor(calls *[]int64) func(string, int64) []byte {
	return func(uid string, generation int64) []byte {
		*calls = append(*calls, generation)
		return Key(testToken + uid + string(rune('0'+generation)))
	}
}

func guestSign(t *testing.T, c Claim) Attestation {
	return signed(t, testToken+c.ActorUID+string(rune('0'+c.Generation)), c)
}

func TestRegistryConsumesEachChallengeOnce(t *testing.T) {
	var calls []int64
	r := newTestRegistry(&clock{time.Unix(1000, 0)}, 8)
	claim, err := r.Issue(testRequest("request-1"))
	if err != nil || claim.Validate() != nil {
		t.Fatal(claim, err)
	}
	second, _ := r.Issue(testRequest("request-2"))
	if second.Challenge == claim.Challenge {
		t.Fatal("challenges repeat")
	}
	got, err := r.Consume(guestSign(t, claim), keyFor(&calls))
	if err != nil || got != claim {
		t.Fatal(got, err)
	}
	if _, err := r.Consume(guestSign(t, claim), keyFor(&calls)); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay: %v", err)
	}
	if _, err := r.Consume(guestSign(t, second), keyFor(&calls)); err != nil {
		t.Fatalf("independent attempt: %v", err)
	}
}

func TestRegistryBurnsChallengeOnAnyConsume(t *testing.T) {
	var calls []int64
	r := newTestRegistry(&clock{time.Unix(1000, 0)}, 8)
	claim, _ := r.Issue(testRequest("request-1"))
	forged := guestSign(t, claim)
	forged.Signature = strings.Repeat("A", 43)
	if _, err := r.Consume(forged, keyFor(&calls)); !errors.Is(err, ErrSignature) {
		t.Fatal(err)
	}
	if _, err := r.Consume(guestSign(t, claim), keyFor(&calls)); !errors.Is(err, ErrReplay) {
		t.Fatalf("challenge survived a failed attempt: %v", err)
	}
}

func TestRegistryNewAttemptReplacesChallenge(t *testing.T) {
	var calls []int64
	r := newTestRegistry(&clock{time.Unix(1000, 0)}, 8)
	old, _ := r.Issue(testRequest("request-1"))
	again, _ := r.Issue(testRequest("request-1"))
	if _, err := r.Consume(guestSign(t, old), keyFor(&calls)); !errors.Is(err, ErrReplay) {
		t.Fatalf("replaced challenge accepted: %v", err)
	}
	if _, err := r.Consume(guestSign(t, again), keyFor(&calls)); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryExpiresChallenges(t *testing.T) {
	var calls []int64
	c := &clock{time.Unix(1000, 0)}
	r := newTestRegistry(c, 8)
	claim, _ := r.Issue(testRequest("request-1"))
	c.now = c.now.Add(15*time.Minute + time.Second)
	if _, err := r.Consume(guestSign(t, claim), keyFor(&calls)); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	if _, err := r.Consume(guestSign(t, claim), keyFor(&calls)); !errors.Is(err, ErrReplay) {
		t.Fatal("expired challenge not burned")
	}
}

// The key comes from the issued identity: a guest of a newer incarnation
// cannot answer a challenge issued to the old one, even with a valid MAC.
func TestRegistryVerifiesWithTheIssuedIncarnation(t *testing.T) {
	var calls []int64
	r := newTestRegistry(&clock{time.Unix(1000, 0)}, 8)
	claim, _ := r.Issue(testRequest("request-1"))
	newer := claim
	newer.Generation = 8
	if _, err := r.Consume(guestSign(t, newer), keyFor(&calls)); !errors.Is(err, ErrIncarnation) {
		t.Fatal(err)
	}
	for _, g := range calls {
		if g != claim.Generation {
			t.Fatalf("key derived for generation %d", g)
		}
	}
}

func TestRegistryBoundsPendingChallenges(t *testing.T) {
	var calls []int64
	c := &clock{time.Unix(1000, 0)}
	r := newTestRegistry(c, 2)
	first, _ := r.Issue(testRequest("request-1"))
	if _, err := r.Issue(testRequest("request-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(testRequest("request-3")); err == nil {
		t.Fatal("pending limit exceeded")
	}
	if _, err := r.Consume(guestSign(t, first), keyFor(&calls)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(testRequest("request-3")); err != nil {
		t.Fatalf("consumed challenge still counted: %v", err)
	}
	c.now = c.now.Add(time.Hour)
	if _, err := r.Issue(testRequest("request-4")); err != nil {
		t.Fatalf("expired challenges still counted: %v", err)
	}
}

// A challenge the guest refused, answered with garbage or never answered is
// revoked by the server: it can no longer be consumed and no longer counts
// against the pending limit.
func TestRegistryRevokeBurnsChallenge(t *testing.T) {
	var calls []int64
	r := newTestRegistry(&clock{time.Unix(1000, 0)}, 1)
	claim, err := r.Issue(testRequest("request-1"))
	if err != nil {
		t.Fatal(err)
	}
	r.Revoke(claim.Challenge)
	if _, err := r.Consume(guestSign(t, claim), keyFor(&calls)); !errors.Is(err, ErrReplay) {
		t.Fatalf("revoked challenge consumed: %v", err)
	}
	if _, err := r.Issue(testRequest("request-2")); err != nil {
		t.Fatalf("revoked challenge still counted: %v", err)
	}
	// Отзыв неизвестного или уже сожжённого challenge ничего не трогает.
	r.Revoke(claim.Challenge)
	r.Revoke("unknown")
	if len(r.pending) != 1 || len(r.attempts) != 1 {
		t.Fatalf("revoke touched other challenges: %d pending, %d attempts", len(r.pending), len(r.attempts))
	}
}

func TestRegistryRefusesInvalidRequestsAndRandomness(t *testing.T) {
	r := newTestRegistry(&clock{time.Unix(1000, 0)}, 8)
	bad := testRequest("request-1")
	bad.Commit = "HEAD"
	if _, err := r.Issue(bad); err == nil {
		t.Fatal("invalid request issued")
	}
	r.random = io.LimitReader(bytes.NewReader(nil), 0)
	if _, err := r.Issue(testRequest("request-1")); err == nil {
		t.Fatal("challenge issued without randomness")
	}
	if _, err := base64.RawURLEncoding.DecodeString(validClaim().Challenge); err != nil {
		t.Fatal(err)
	}
}
