package attest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const inboxLaunchMessageLabel = "brood/proxmox/inbox-launch/v1"
const inboxLaunchKeyLabel = "brood/proxmox/inbox-launch/key/v1"

// MaxInboxLaunchProofBytes bounds one encoded inbox launch proof.
const MaxInboxLaunchProofBytes = 4096

// InboxLaunchClaim binds the exact verified workspace and root-held launch ID
// to the selected supervisor inbox receiver. LaunchID is an identifier, not a
// bearer credential, and does not assert model admission.
type InboxLaunchClaim struct {
	Claim
	LaunchID   string `json:"launch_id"`
	ReceiverID string `json:"receiver_id"`
}

// InboxLaunchProof is signed evidence of launch identity and custody capability,
// not evidence that a model was admitted or consumed an instruction.
type InboxLaunchProof struct {
	InboxLaunchClaim
	Signature string `json:"signature"`
}

// Validate bounds the workspace fields and requires a root launch ID and receiver ID.
func (c InboxLaunchClaim) Validate() error {
	if len(c.Commit) != 40 {
		return errors.New("inbox launch commit must be a full lowercase SHA-1")
	}
	if len(c.Challenge) != base64.RawURLEncoding.EncodedLen(ChallengeBytes) {
		return errors.New("inbox launch challenge must be 32 bytes of unpadded base64url")
	}
	if err := c.Claim.Validate(); err != nil {
		return err
	}
	// rand.Text uses unpadded base32 and may increase its output length.
	if len(c.LaunchID) < 26 || len(c.LaunchID) > 128 {
		return errors.New("inbox launch ID is invalid")
	}
	for _, ch := range c.LaunchID {
		if !(ch >= 'A' && ch <= 'Z' || ch >= '2' && ch <= '7') {
			return errors.New("inbox launch ID is invalid")
		}
	}
	if len(c.ReceiverID) != 64 {
		return errors.New("inbox receiver ID is invalid")
	}
	for _, ch := range c.ReceiverID {
		if !(ch >= 'a' && ch <= 'f' || ch >= '0' && ch <= '9') {
			return errors.New("inbox receiver ID is invalid")
		}
	}
	return nil
}

// InboxLaunchKey derives the launch proof key from the expected incarnation's
// guest relay token in a domain separate from workspace and result attestations.
func InboxLaunchKey(token string) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(inboxLaunchKeyLabel))
	return mac.Sum(nil)
}

// SignInboxLaunch returns an unpadded base64url HMAC-SHA256. The signer must
// independently verify the workspace and obtain both IDs from the root supervisor.
func SignInboxLaunch(key []byte, c InboxLaunchClaim) (string, error) {
	if len(key) == 0 {
		return "", errors.New("inbox launch key is required")
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(inboxLaunchMAC(key, c)), nil
}

func inboxLaunchMAC(key []byte, c InboxLaunchClaim) []byte {
	m := hmac.New(sha256.New, key)
	fields := []string{inboxLaunchMessageLabel, c.ActorUID, strconv.FormatInt(c.Generation, 10), c.Swarm, c.Step, c.Origin, c.Commit, c.Challenge, c.LaunchID, c.ReceiverID}
	m.Write([]byte(strings.Join(fields, "\x00")))
	return m.Sum(nil)
}

// VerifyInboxLaunch checks proof against the exact claim from the caller's
// one-time workspace challenge consumption and the independently selected live
// receiver ID. The key must derive from want's identity, not the proof. This
// verifier is stateless; the caller must fence replay and recheck receiver and
// launch lifetime before using the binding.
func VerifyInboxLaunch(key []byte, want Claim, receiverID string, got InboxLaunchProof) error {
	if len(key) == 0 {
		return errors.New("inbox launch key is required")
	}
	switch {
	case got.ActorUID != want.ActorUID:
		return fmt.Errorf("%w: actor", ErrMismatch)
	case got.Generation != want.Generation:
		return ErrIncarnation
	case got.Claim != want:
		return ErrMismatch
	case got.ReceiverID != receiverID:
		return fmt.Errorf("%w: inbox receiver", ErrMismatch)
	}
	if err := got.InboxLaunchClaim.Validate(); err != nil {
		return err
	}
	if sig := inboxLaunchSignature(got.Signature); sig == nil || !hmac.Equal(sig, inboxLaunchMAC(key, got.InboxLaunchClaim)) {
		return ErrSignature
	}
	return nil
}

func inboxLaunchSignature(text string) []byte {
	if len(text) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return nil
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(text)
	if err != nil || len(sig) != sha256.Size {
		return nil
	}
	return sig
}

// DecodeInboxLaunchProof parses one bounded proof with exact fields. It does
// not authenticate the signature; callers must also use VerifyInboxLaunch.
func DecodeInboxLaunchProof(data []byte) (InboxLaunchProof, error) {
	var proof InboxLaunchProof
	err := proof.UnmarshalJSON(data)
	return proof, err
}

// UnmarshalJSON enforces the same bounded proof format when embedded in a response.
func (p *InboxLaunchProof) UnmarshalJSON(data []byte) error {
	fail := errors.New("malformed inbox launch proof")
	if len(data) > MaxInboxLaunchProofBytes || !utf8.Valid(data) || !uniqueFields(data) {
		return fail
	}
	keys := [...]string{"actor_uid", "generation", "swarm", "step", "origin", "commit", "challenge", "launch_id", "receiver_id", "signature"}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != len(keys) {
		return fail
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return fail
		}
	}
	type plain InboxLaunchProof
	var decoded plain
	if json.Unmarshal(data, &decoded) != nil || decoded.InboxLaunchClaim.Validate() != nil || inboxLaunchSignature(decoded.Signature) == nil {
		return fail
	}
	*p = InboxLaunchProof(decoded)
	return nil
}
