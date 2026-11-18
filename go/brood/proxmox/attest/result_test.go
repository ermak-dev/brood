package attest

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func resultClaim() ResultClaim {
	return ResultClaim{ResultRequest: ResultRequest{Claim: validClaim(), Pass: 1, Sequence: 2}, Head: strings.Repeat("cd", 20), Paths: []string{"go/store/a.go", "go/store/b.go"}}
}

func TestResultSignatureBindsObservedArtifactAndExactRequest(t *testing.T) {
	claim := resultClaim()
	key := ResultKey(testToken)
	signature, err := SignResult(key, claim)
	if err != nil {
		t.Fatal(err)
	}
	signed := ResultAttestation{ResultClaim: claim, Signature: signature}
	if err := VerifyResult(key, claim.ResultRequest, signed); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ResultClaim){
		"actor":       func(c *ResultClaim) { c.ActorUID = "another" },
		"incarnation": func(c *ResultClaim) { c.Generation++ },
		"swarm":       func(c *ResultClaim) { c.Swarm = "another" },
		"attempt":     func(c *ResultClaim) { c.Step = "another" },
		"origin":      func(c *ResultClaim) { c.Origin = "https://git.example.com/other/project" },
		"base":        func(c *ResultClaim) { c.Commit = strings.Repeat("ef", 20) },
		"head":        func(c *ResultClaim) { c.Head = strings.Repeat("ef", 20) },
		"pass":        func(c *ResultClaim) { c.Pass = 2 },
		"sequence":    func(c *ResultClaim) { c.Sequence = 3 },
		"paths":       func(c *ResultClaim) { c.Paths = []string{"docs/readme.md"} },
		"challenge":   func(c *ResultClaim) { c.Challenge = strings.Repeat("e", 43) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := signed
			change(&changed.ResultClaim)
			if err := VerifyResult(key, claim.ResultRequest, changed); err == nil {
				t.Fatal("altered result accepted")
			}
		})
	}
	if err := VerifyResult(Key(testToken), claim.ResultRequest, signed); !errors.Is(err, ErrSignature) {
		t.Fatalf("v1 key accepted: %v", err)
	}
	wire, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeResultAttestation(wire)
	if err != nil || !reflect.DeepEqual(decoded, signed) {
		t.Fatalf("result roundtrip: %+v %v", decoded, err)
	}
}

func TestResultAttestationRejectsAmbiguousWireAndIncompletePaths(t *testing.T) {
	claim := resultClaim()
	sig, err := SignResult(ResultKey(testToken), claim)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(ResultAttestation{ResultClaim: claim, Signature: sig})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		string(wire) + ` {}`,
		strings.Replace(string(wire), `"head":`, `"Head":`, 1),
		strings.Replace(string(wire), `"head":`, `"unknown":`, 1),
		strings.Replace(string(wire), `"pass":1`, `"pass":1,"pass":2`, 1),
		strings.Replace(string(wire), `"paths":["go/store/a.go","go/store/b.go"]`, `"paths":null`, 1),
		strings.Replace(string(wire), `"sequence":2,`, "", 1),
	} {
		if _, err := DecodeResultAttestation([]byte(raw)); err == nil {
			t.Fatal("ambiguous result wire accepted")
		}
	}
	for _, paths := range [][]string{nil, {"../escape"}, {"a", "a"}, {"b", "a"}, {"a\x00b"}, {"/absolute"}, {strings.Repeat("x", 1025)}} {
		bad := claim
		bad.Paths = paths
		if _, err := SignResult(ResultKey(testToken), bad); err == nil {
			t.Fatal("invalid path evidence signed")
		}
	}
	empty := claim
	empty.Paths = []string{}
	if _, err := SignResult(ResultKey(testToken), empty); err != nil {
		t.Fatal("explicit complete empty diff rejected")
	}
}
