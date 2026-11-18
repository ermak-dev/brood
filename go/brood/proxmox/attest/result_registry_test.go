package attest

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func resultRegistryRequest() Request {
	c := validClaim()
	return Request{ActorUID: c.ActorUID, Generation: c.Generation, Swarm: c.Swarm, Step: c.Step, Origin: c.Origin, Commit: c.Commit}
}
func signRegistryResult(t *testing.T, request ResultRequest) ResultAttestation {
	t.Helper()
	claim := resultClaim()
	claim.ResultRequest = request
	signature, err := SignResult(ResultKey(testToken), claim)
	if err != nil {
		t.Fatal(err)
	}
	return ResultAttestation{ResultClaim: claim, Signature: signature}
}
func TestResultRegistryConsumesOnlyExactFreshResultOnce(t *testing.T) {
	registry := NewRegistry(time.Minute, 4)
	request, err := registry.IssueResult(resultRegistryRequest(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	signed := signRegistryResult(t, request)
	key := func(uid string, generation int64) []byte {
		if uid != request.ActorUID || generation != request.Generation {
			t.Fatal("key chosen from untrusted identity")
		}
		return ResultKey(testToken)
	}
	got, err := registry.ConsumeResult(signed, key)
	if err != nil || !reflect.DeepEqual(got, signed.ResultClaim) {
		t.Fatalf("result rejected: %+v %v", got, err)
	}
	if _, err := registry.ConsumeResult(signed, key); !errors.Is(err, ErrReplay) {
		t.Fatalf("result replay: %v", err)
	}
}

func TestResultRegistryBurnsForgeryAndExpiresRequests(t *testing.T) {
	for _, kind := range []string{"forged", "other-step", "expired", "replaced", "revoked"} {
		t.Run(kind, func(t *testing.T) {
			registry := NewRegistry(time.Minute, 4)
			now := time.Now()
			registry.now = func() time.Time { return now }
			request, err := registry.IssueResult(resultRegistryRequest(), 1, 2)
			if err != nil {
				t.Fatal(err)
			}
			signed := signRegistryResult(t, request)
			want := ErrSignature
			switch kind {
			case "forged":
				signed.Head = validClaim().Commit
			case "other-step":
				signed.Sequence = 3
				want = ErrMismatch
			case "expired":
				now = now.Add(time.Minute)
				want = ErrExpired
			case "replaced":
				if _, err := registry.IssueResult(resultRegistryRequest(), 1, 2); err != nil {
					t.Fatal(err)
				}
				want = ErrReplay
			case "revoked":
				registry.Revoke(request.Challenge)
				want = ErrReplay
			}
			key := func(string, int64) []byte { return ResultKey(testToken) }
			if _, err := registry.ConsumeResult(signed, key); !errors.Is(err, want) {
				t.Fatalf("%s accepted or wrong refusal: %v", kind, err)
			}
			if _, err := registry.ConsumeResult(signRegistryResult(t, request), key); !errors.Is(err, ErrReplay) {
				t.Fatalf("challenge survived failed consumption: %v", err)
			}
		})
	}
}

func TestResultRegistryDoesNotConfuseCheckoutAndResultChallenges(t *testing.T) {
	registry := NewRegistry(time.Minute, 4)
	request, err := registry.IssueResult(resultRegistryRequest(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := Sign(Key(testToken), request.Claim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Consume(Attestation{Claim: request.Claim, Signature: signature}, func(string, int64) []byte { return Key(testToken) }); !errors.Is(err, ErrMismatch) {
		t.Fatalf("result challenge consumed as checkout: %v", err)
	}
	claim, err := registry.Issue(resultRegistryRequest())
	if err != nil {
		t.Fatal(err)
	}
	result := ResultRequest{Claim: claim, Pass: 1, Sequence: 2}
	if _, err := registry.ConsumeResult(signRegistryResult(t, result), func(string, int64) []byte { return ResultKey(testToken) }); !errors.Is(err, ErrMismatch) {
		t.Fatalf("checkout challenge consumed as result: %v", err)
	}
}
