package attest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

const testToken = "guest-token-of-actor-1-generation-7-xxxxxxx"

func validClaim() Claim {
	return Claim{ActorUID: "actor-1", Generation: 7, Swarm: "01JSWARM0000000000000000AA", Step: "request-1", Origin: "https://git.example.com/group/project", Commit: strings.Repeat("ab", 20), Challenge: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
}

// The wire format is shared by the guest image and the server; a silent change
// would make every deployed guest fail verification, so it is pinned here.
func TestSignaturePinsWireFormat(t *testing.T) {
	c := validClaim()
	keyMAC := hmac.New(sha256.New, []byte(testToken))
	keyMAC.Write([]byte("brood/proxmox/workspace-attestation/key/v1"))
	key := keyMAC.Sum(nil)
	if got := Key(testToken); !hmac.Equal(got, key) {
		t.Fatal("key derivation changed")
	}
	message := "brood/proxmox/workspace-attestation/v1\x00actor-1\x007\x0001JSWARM0000000000000000AA\x00request-1\x00https://git.example.com/group/project\x00" + c.Commit + "\x00" + c.Challenge
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	got, err := Sign(key, c)
	if err != nil || got != want {
		t.Fatalf("signature %q %v, want %q", got, err, want)
	}
}

func TestSignatureBindsEveryField(t *testing.T) {
	key := Key(testToken)
	base, err := Sign(key, validClaim())
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Claim){
		"actor":      func(c *Claim) { c.ActorUID = "actor-2" },
		"generation": func(c *Claim) { c.Generation = 8 },
		"swarm":      func(c *Claim) { c.Swarm = "01JSWARM0000000000000000AB" },
		"step":       func(c *Claim) { c.Step = "request-2" },
		"origin":     func(c *Claim) { c.Origin = "https://git.example.com/group/other" },
		"commit":     func(c *Claim) { c.Commit = strings.Repeat("cd", 20) },
		"challenge":  func(c *Claim) { c.Challenge = base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32))) },
	} {
		c := validClaim()
		change(&c)
		got, err := Sign(key, c)
		if err != nil || got == base {
			t.Errorf("%s: signature did not change (%v)", name, err)
		}
	}
	if other, _ := Sign(Key(testToken+"2"), validClaim()); other == base {
		t.Fatal("signature does not depend on the incarnation token")
	}
	if string(Key(testToken)) == testToken {
		t.Fatal("token used as the attestation key")
	}
}

func TestSignRejectsInvalidClaims(t *testing.T) {
	for name, change := range map[string]func(*Claim){
		"empty actor":       func(c *Claim) { c.ActorUID = "" },
		"actor separator":   func(c *Claim) { c.ActorUID = "actor\x00x" },
		"actor slash":       func(c *Claim) { c.ActorUID = "actor/1" },
		"zero generation":   func(c *Claim) { c.Generation = 0 },
		"negative":          func(c *Claim) { c.Generation = -1 },
		"empty swarm":       func(c *Claim) { c.Swarm = "" },
		"swarm separator":   func(c *Claim) { c.Swarm = "a\x00b" },
		"long swarm":        func(c *Claim) { c.Swarm = strings.Repeat("a", 129) },
		"empty step":        func(c *Claim) { c.Step = "" },
		"step newline":      func(c *Claim) { c.Step = "a\nb" },
		"long step":         func(c *Claim) { c.Step = strings.Repeat("a", 129) },
		"origin http":       func(c *Claim) { c.Origin = "http://git.example.com/group/project" },
		"origin noncanon":   func(c *Claim) { c.Origin = "https://GIT.example.com/group/project/" },
		"empty origin":      func(c *Claim) { c.Origin = "" },
		"short commit":      func(c *Claim) { c.Commit = strings.Repeat("a", 39) },
		"upper commit":      func(c *Claim) { c.Commit = strings.Repeat("AB", 20) },
		"sha256 commit":     func(c *Claim) { c.Commit = strings.Repeat("ab", 32) },
		"empty challenge":   func(c *Claim) { c.Challenge = "" },
		"short challenge":   func(c *Claim) { c.Challenge = base64.RawURLEncoding.EncodeToString(make([]byte, 16)) },
		"padded challenge":  func(c *Claim) { c.Challenge = base64.URLEncoding.EncodeToString(make([]byte, 32)) },
		"std alphabet":      func(c *Claim) { c.Challenge = strings.Repeat("+", 43) },
		"noncanonical bits": func(c *Claim) { c.Challenge = strings.Repeat("A", 42) + "B" },
	} {
		c := validClaim()
		change(&c)
		if _, err := Sign(Key(testToken), c); err == nil {
			t.Errorf("%s: signed an invalid claim", name)
		}
		if c.Validate() == nil {
			t.Errorf("%s: validated an invalid claim", name)
		}
	}
	if _, err := Sign(nil, validClaim()); err == nil {
		t.Fatal("signed without a key")
	}
	if err := validClaim().Validate(); err != nil {
		t.Fatal(err)
	}
}
