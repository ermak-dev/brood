// Package attest signs and verifies workspace attestations of managed Proxmox
// actors. The guest root supervisor signs; the server verifies. It is pure:
// no network, storage or guest effects.
package attest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"brood.dev/go/brood/gitorigin"
)

// Метки версионируют и формат сообщения, и вывод ключа: смена любой из них
// ломает проверку у всех выкаченных гостей, поэтому формат закреплён тестом.
const messageLabel = "brood/proxmox/workspace-attestation/v1"
const keyLabel = "brood/proxmox/workspace-attestation/key/v1"

// ChallengeBytes is the size of a server challenge before base64url encoding.
const ChallengeBytes = 32

// Claim is what the guest root asserts after it verified the workspace while no
// actor process existed. ActorUID and Generation come from the guest's own
// bootstrap identity, never from the request.
type Claim struct {
	ActorUID   string `json:"actor_uid"`
	Generation int64  `json:"generation"`
	Swarm      string `json:"swarm"`
	// Step identifies one delivery attempt (its request ID), not a node.
	Step      string `json:"step"`
	Origin    string `json:"origin"`
	Commit    string `json:"commit"`
	Challenge string `json:"challenge"`
}

// Attestation is the signed guest answer.
type Attestation struct {
	Claim
	Signature string `json:"signature"`
}

// Validate bounds every field so that NUL-separated encoding is injective.
func (c Claim) Validate() error {
	switch {
	case !identifier(c.ActorUID, false):
		return errors.New("attestation actor UID is invalid")
	case c.Generation <= 0:
		return errors.New("attestation incarnation must be positive")
	case !identifier(c.Swarm, false):
		return errors.New("attestation swarm ID is invalid")
	case !identifier(c.Step, true):
		return errors.New("attestation step attempt ID is invalid")
	case !canonicalOrigin(c.Origin):
		return errors.New("attestation origin must be a canonical HTTPS origin")
	case !fullCommit(c.Commit):
		return errors.New("attestation commit must be a full lowercase SHA-1")
	case !validChallenge(c.Challenge):
		return errors.New("attestation challenge must be 32 bytes of unpadded base64url")
	}
	return nil
}

// Key derives the attestation key from the incarnation's guest relay token.
// The server recomputes the token from its relay secret; no new secret exists.
func Key(token string) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(keyLabel))
	return mac.Sum(nil)
}

// Sign returns the unpadded base64url HMAC-SHA256 of a valid claim.
func Sign(key []byte, c Claim) (string, error) {
	if len(key) == 0 {
		return "", errors.New("attestation key is required")
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(mac(key, c)), nil
}

func mac(key []byte, c Claim) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strings.Join([]string{messageLabel, c.ActorUID, strconv.FormatInt(c.Generation, 10), c.Swarm, c.Step, c.Origin, c.Commit, c.Challenge}, "\x00")))
	return m.Sum(nil)
}

// identifier accepts 1..128 bytes of [A-Za-z0-9_-]; attempt IDs also allow
// '.' and ':' used by request identifiers.
func identifier(s string, attempt bool) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok && !(attempt && (r == '.' || r == ':')) {
			return false
		}
	}
	return true
}

func canonicalOrigin(origin string) bool {
	canonical, err := gitorigin.Canonical(origin)
	return err == nil && canonical == origin
}

func fullCommit(commit string) bool {
	raw, err := hex.DecodeString(commit)
	return err == nil && len(raw) == 20 && commit == strings.ToLower(commit)
}

func validChallenge(challenge string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(challenge)
	return err == nil && len(raw) == ChallengeBytes
}
