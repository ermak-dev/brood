package attest_test

// FEATURE: A root-signed launch identity bound to one workspace claim and inbox receiver.
// SPEC: Pin an independent wire fixture; reject mismatched scope, replay in the
// caller's workspace challenge registry, and malformed proofs without changing legacy wire.
// IMPLEMENTATION: Use separate HMAC domains and bounded, exact proof fields in an optional response member.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/proxmox/attest"
	"brood.dev/go/brood/proxmox/relay"
)

const (
	inboxFixtureToken = "inbox-launch-fixture-token"
	inboxFixtureKey   = "65a22a2aa8eb99c061c931eb7dc7dfd60205d4d08f528f9d34742b7fd5b484c7"
	inboxFixtureSig   = "WeR7Yv6nE-QN5J58cjn7_sW4T03bcdOcDps9lh4zP38"
	inboxFixtureWire  = `{"actor_uid":"actor-1","generation":7,"swarm":"swarm-3","step":"request:step.1","origin":"https://git.example.com/group/project","commit":"abababababababababababababababababababab","challenge":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8","launch_id":"ABCDEFGHIJKLMNOPQRSTUVWXYZ","receiver_id":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","signature":"` + inboxFixtureSig + `"}`
)

func inboxFixture() attest.InboxLaunchProof {
	return attest.InboxLaunchProof{
		InboxLaunchClaim: attest.InboxLaunchClaim{
			Claim: attest.Claim{
				ActorUID: "actor-1", Generation: 7, Swarm: "swarm-3", Step: "request:step.1",
				Origin: "https://git.example.com/group/project", Commit: strings.Repeat("ab", 20),
				Challenge: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
			},
			LaunchID: "ABCDEFGHIJKLMNOPQRSTUVWXYZ", ReceiverID: strings.Repeat("0123456789abcdef", 4),
		},
		Signature: inboxFixtureSig,
	}
}

func TestInboxLaunchFixture(t *testing.T) {
	want := inboxFixture()
	key := attest.InboxLaunchKey(inboxFixtureToken)
	if hex.EncodeToString(key) != inboxFixtureKey {
		t.Fatal("inbox launch key derivation changed")
	}
	sig, err := attest.SignInboxLaunch(key, want.InboxLaunchClaim)
	if err != nil || sig != inboxFixtureSig {
		t.Fatalf("fixture signature = %q, %v", sig, err)
	}
	proof, err := attest.DecodeInboxLaunchProof([]byte(inboxFixtureWire))
	if err != nil || proof != want {
		t.Fatalf("fixture decoded to %#v, %v", proof, err)
	}
	if err := attest.VerifyInboxLaunch(key, want.Claim, want.ReceiverID, proof); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(proof)
	if err != nil || string(wire) != inboxFixtureWire {
		t.Fatalf("proof wire = %s, %v", wire, err)
	}
}

func TestInboxLaunchBindsExactContextAndLaunch(t *testing.T) {
	proof := inboxFixture()
	key := attest.InboxLaunchKey(inboxFixtureToken)
	for _, tc := range []struct {
		name   string
		mutate func(*attest.InboxLaunchClaim)
		err    error
	}{
		{"actor", func(c *attest.InboxLaunchClaim) { c.ActorUID = "actor-2" }, attest.ErrMismatch},
		{"incarnation", func(c *attest.InboxLaunchClaim) { c.Generation++ }, attest.ErrIncarnation},
		{"swarm", func(c *attest.InboxLaunchClaim) { c.Swarm = "other-swarm" }, attest.ErrMismatch},
		{"step", func(c *attest.InboxLaunchClaim) { c.Step = "other-step" }, attest.ErrMismatch},
		{"origin", func(c *attest.InboxLaunchClaim) { c.Origin = "https://git.example.com/group/other" }, attest.ErrMismatch},
		{"commit", func(c *attest.InboxLaunchClaim) { c.Commit = strings.Repeat("cd", 20) }, attest.ErrMismatch},
		{"challenge", func(c *attest.InboxLaunchClaim) { c.Challenge = strings.Repeat("A", 43) }, attest.ErrMismatch},
		{"receiver", func(c *attest.InboxLaunchClaim) { c.ReceiverID = strings.Repeat("a", 64) }, attest.ErrMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := proof
			tc.mutate(&changed.InboxLaunchClaim)
			if err := attest.VerifyInboxLaunch(key, changed.Claim, changed.ReceiverID, proof); !errors.Is(err, tc.err) {
				t.Fatalf("stale or wrong proof did not refuse changed context: %v", err)
			}
			if err := attest.VerifyInboxLaunch(key, changed.Claim, changed.ReceiverID, changed); !errors.Is(err, attest.ErrSignature) {
				t.Fatalf("tampered proof fit changed context: %v", err)
			}
		})
	}
	changed := proof
	changed.LaunchID = strings.Repeat("Z", 26)
	if err := attest.VerifyInboxLaunch(key, proof.Claim, proof.ReceiverID, changed); !errors.Is(err, attest.ErrSignature) {
		t.Fatalf("replaced launch was not rejected: %v", err)
	}
	if err := attest.VerifyInboxLaunch(attest.InboxLaunchKey("other-fixture-token"), proof.Claim, proof.ReceiverID, proof); !errors.Is(err, attest.ErrSignature) {
		t.Fatalf("wrong incarnation key was not rejected: %v", err)
	}
}

func TestInboxLaunchDomainsDoNotInteroperate(t *testing.T) {
	proof := inboxFixture()
	key := attest.InboxLaunchKey(inboxFixtureToken)
	nativeKey, resultKey := attest.Key(inboxFixtureToken), attest.ResultKey(inboxFixtureToken)
	for _, other := range [][]byte{nativeKey, resultKey} {
		if err := attest.VerifyInboxLaunch(other, proof.Claim, proof.ReceiverID, proof); !errors.Is(err, attest.ErrSignature) {
			t.Fatalf("wrong key domain accepted: %v", err)
		}
	}
	// Use one key on purpose to isolate message-domain separation.
	native := attest.Attestation{Claim: proof.Claim}
	result := attest.ResultAttestation{ResultClaim: attest.ResultClaim{
		ResultRequest: attest.ResultRequest{Claim: proof.Claim, Pass: 1, Sequence: 1},
		Head:          proof.Commit, Paths: []string{},
	}}
	var err error
	native.Signature, err = attest.Sign(key, native.Claim)
	if err != nil {
		t.Fatal(err)
	}
	result.Signature, err = attest.SignResult(key, result.ResultClaim)
	if err != nil {
		t.Fatal(err)
	}
	for _, sig := range []string{native.Signature, result.Signature} {
		replaced := proof
		replaced.Signature = sig
		if err := attest.VerifyInboxLaunch(key, proof.Claim, proof.ReceiverID, replaced); !errors.Is(err, attest.ErrSignature) {
			t.Fatalf("foreign message domain accepted: %v", err)
		}
	}
	native.Signature, result.Signature = proof.Signature, proof.Signature
	if err := attest.Verify(key, proof.Claim, native); !errors.Is(err, attest.ErrSignature) {
		t.Fatalf("inbox proof treated as workspace attestation: %v", err)
	}
	if err := attest.VerifyResult(key, result.ResultRequest, result); !errors.Is(err, attest.ErrSignature) {
		t.Fatalf("inbox proof treated as result attestation: %v", err)
	}
}

func TestInboxLaunchRejectsInvalidScopeAndSignatures(t *testing.T) {
	proof := inboxFixture()
	key := attest.InboxLaunchKey(inboxFixtureToken)
	for _, mutate := range []func(*attest.InboxLaunchClaim){
		func(c *attest.InboxLaunchClaim) { c.LaunchID = "" },
		func(c *attest.InboxLaunchClaim) { c.LaunchID = strings.Repeat("A", 25) },
		func(c *attest.InboxLaunchClaim) { c.LaunchID = strings.Repeat("A", 129) },
		func(c *attest.InboxLaunchClaim) { c.LaunchID = "0" + c.LaunchID[1:] },
		func(c *attest.InboxLaunchClaim) { c.LaunchID = "a" + c.LaunchID[1:] },
		func(c *attest.InboxLaunchClaim) { c.LaunchID = "\x00" + c.LaunchID[1:] },
		func(c *attest.InboxLaunchClaim) { c.ReceiverID = "" },
		func(c *attest.InboxLaunchClaim) { c.ReceiverID = c.ReceiverID[1:] },
		func(c *attest.InboxLaunchClaim) { c.ReceiverID += "f" },
		func(c *attest.InboxLaunchClaim) { c.ReceiverID = strings.ToUpper(c.ReceiverID) },
		func(c *attest.InboxLaunchClaim) { c.Challenge += "\n" },
		func(c *attest.InboxLaunchClaim) { c.Origin = "https://user:private@git.example.com/group/project" },
	} {
		changed := proof
		mutate(&changed.InboxLaunchClaim)
		if sig, err := attest.SignInboxLaunch(key, changed.InboxLaunchClaim); err == nil || sig != "" {
			t.Fatal("invalid launch scope signed")
		}
		if err := attest.VerifyInboxLaunch(key, proof.Claim, proof.ReceiverID, changed); err == nil {
			t.Fatal("invalid launch scope verified")
		}
	}
	proof.LaunchID = strings.Repeat("7", 128)
	if _, err := attest.SignInboxLaunch(key, proof.InboxLaunchClaim); err != nil {
		t.Fatalf("upper launch ID bound rejected: %v", err)
	}
	if sig, err := attest.SignInboxLaunch(nil, proof.InboxLaunchClaim); err == nil || sig != "" {
		t.Fatal("proof signed without key")
	}
	if err := attest.VerifyInboxLaunch(nil, proof.Claim, proof.ReceiverID, inboxFixture()); err == nil {
		t.Fatal("proof verified without key")
	}
	if err := attest.VerifyInboxLaunch(key, proof.Claim, "", inboxFixture()); err == nil {
		t.Fatal("proof verified without an independently supplied receiver")
	}
	for _, sig := range []string{"", inboxFixtureSig + "=", inboxFixtureSig + "\n", inboxFixtureSig[:42] + "9"} {
		changed := inboxFixture()
		changed.Signature = sig
		if err := attest.VerifyInboxLaunch(key, changed.Claim, changed.ReceiverID, changed); !errors.Is(err, attest.ErrSignature) {
			t.Fatalf("malformed signature accepted: %v", err)
		}
	}
}

func TestInboxLaunchConsumesCallerWorkspaceChallenge(t *testing.T) {
	proof := inboxFixture()
	registry := attest.NewRegistry(time.Minute, 1)
	want, err := registry.Issue(attest.Request{
		ActorUID: proof.ActorUID, Generation: proof.Generation, Swarm: proof.Swarm,
		Step: proof.Step, Origin: proof.Origin, Commit: proof.Commit,
	})
	if err != nil {
		t.Fatal(err)
	}
	proof.Claim = want
	key := attest.InboxLaunchKey(inboxFixtureToken)
	proof.Signature, err = attest.SignInboxLaunch(key, proof.InboxLaunchClaim)
	if err != nil {
		t.Fatal(err)
	}
	workspaceSig, err := attest.Sign(attest.Key(inboxFixtureToken), want)
	if err != nil {
		t.Fatal(err)
	}
	workspace := attest.Attestation{Claim: want, Signature: workspaceSig}
	accept := func() error {
		verified, err := registry.Consume(workspace, func(string, int64) []byte { return attest.Key(inboxFixtureToken) })
		if err != nil {
			return err
		}
		return attest.VerifyInboxLaunch(key, verified, inboxFixture().ReceiverID, proof)
	}
	if err := accept(); err != nil {
		t.Fatal(err)
	}
	if err := accept(); !errors.Is(err, attest.ErrReplay) {
		t.Fatalf("replayed launch proof not refused by caller: %v", err)
	}
}

func TestInboxLaunchStrictDecodeAndOptionalSessionWire(t *testing.T) {
	proof := inboxFixture()
	oldWire := `{"ok":true,"text":"legacy","outcome":"complete"}`
	response := relay.SessionResponse{OK: true, Text: "legacy", Outcome: "complete"}
	for _, candidate := range []struct {
		wire  string
		proof *attest.InboxLaunchProof
	}{
		{oldWire, nil},
		{oldWire[:len(oldWire)-1] + `,"inbox_launch":` + inboxFixtureWire + `}`, &proof},
	} {
		response.InboxLaunch = candidate.proof
		wire, err := json.Marshal(response)
		if err != nil || string(wire) != candidate.wire {
			t.Fatalf("session response wire = %s, %v", wire, err)
		}
		var decoded relay.SessionResponse
		if err := json.Unmarshal(wire, &decoded); err != nil || decoded.OK != response.OK || decoded.Text != response.Text || decoded.Outcome != response.Outcome {
			t.Fatalf("session response round trip = %#v, %v", decoded, err)
		}
		if candidate.proof == nil && decoded.InboxLaunch != nil || candidate.proof != nil && (decoded.InboxLaunch == nil || *decoded.InboxLaunch != proof) {
			t.Fatal("optional inbox proof did not round trip")
		}
	}
	prefix := inboxFixtureWire[:len(inboxFixtureWire)-1]
	padding := strings.Repeat(" ", attest.MaxInboxLaunchProofBytes-len(inboxFixtureWire))
	if _, err := attest.DecodeInboxLaunchProof([]byte(prefix + padding + "}")); err != nil {
		t.Fatalf("proof exactly at size limit rejected: %v", err)
	}
	for _, raw := range []string{
		"", "null", "[]", "{}", prefix + padding + " }", inboxFixtureWire + "{}",
		strings.Replace(inboxFixtureWire, `"generation":7`, `"generation":7,"generation":7`, 1),
		strings.Replace(inboxFixtureWire, `"generation":7`, `"Generation":7`, 1),
		strings.Replace(inboxFixtureWire, `"generation":7,`, "", 1),
		strings.Replace(inboxFixtureWire, `"generation":7`, `"generation":null`, 1),
		strings.Replace(inboxFixtureWire, inboxFixtureSig, "", 1),
		strings.Replace(inboxFixtureWire, proof.LaunchID, "\xff", 1),
		inboxFixtureWire[:len(inboxFixtureWire)-1] + `,"model_admitted":true}`,
	} {
		if _, err := attest.DecodeInboxLaunchProof([]byte(raw)); err == nil {
			t.Fatal("malformed inbox proof decoded")
		}
		if raw == "null" { // A null pointer is absence, not a proof.
			continue
		}
		var decoded relay.SessionResponse
		if err := json.Unmarshal([]byte(`{"outcome":"complete","inbox_launch":`+raw+`}`), &decoded); err == nil {
			t.Fatal("relay accepted malformed inbox proof")
		}
	}
}
