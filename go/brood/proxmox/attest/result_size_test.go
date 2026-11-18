package attest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSignResultRejectsUndecodableEncodedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, character string
		repetitions     int
		oversized       bool
	}{
		{"quoted-fit", `"`, 100, false},
		{"quoted-overflow", `"`, 200, true},
		{"html-overflow", "<", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := resultClaim()
			claim.Paths = make([]string, 300)
			for i := range claim.Paths {
				claim.Paths[i] = fmt.Sprintf("%03d%s", i, strings.Repeat(tc.character, tc.repetitions))
			}
			if err := claim.Validate(); err != nil {
				t.Fatalf("invalid raw evidence fixture: %v", err)
			}
			probe, err := json.Marshal(ResultAttestation{ResultClaim: claim, Signature: strings.Repeat("a", 43)})
			if err != nil || (len(probe) > MaxResultAttestationBytes) != tc.oversized {
				t.Fatalf("incorrect encoded-size fixture: bytes=%d error=%v", len(probe), err)
			}
			key := ResultKey(testToken)
			signature, err := SignResult(key, claim)
			if tc.oversized {
				if err == nil || signature != "" {
					t.Fatalf("signed %d-byte evidence that the decoder cannot accept", len(probe))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(ResultAttestation{ResultClaim: claim, Signature: signature})
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeResultAttestation(wire)
			if err != nil {
				t.Fatalf("signable evidence cannot round trip: %v", err)
			}
			if err := VerifyResult(key, claim.ResultRequest, decoded); err != nil {
				t.Fatal(err)
			}
		})
	}
}
