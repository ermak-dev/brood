package attest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

const resultMessageLabel = "brood/proxmox/workspace-result/v1"
const resultKeyLabel = "brood/proxmox/workspace-result/key/v1"

// MaxResultAttestationBytes bounds one signed complete result observation.
const MaxResultAttestationBytes = 96 << 10

// ResultRequest binds an observation to an exact pass, sequence and delivery attempt.
// Claim.Commit is the authenticated input commit, not the observed result HEAD.
type ResultRequest struct {
	Claim
	Pass     int64 `json:"pass"`
	Sequence int64 `json:"sequence"`
}

// Validate rejects requests that cannot identify one exact started step.
func (r ResultRequest) Validate() error {
	if err := r.Claim.Validate(); err != nil {
		return err
	}
	if r.Pass <= 0 || r.Sequence < r.Pass {
		return errors.New("result pass and step sequence are invalid")
	}
	return nil
}

// ResultClaim is complete root-observed result evidence; Paths must be sorted and unique.
// This signature proves an observation window, not later workspace state or remote publication.
type ResultClaim struct {
	ResultRequest
	Head  string   `json:"head"`
	Paths []string `json:"paths"`
}

// ResultAttestation is separate from input-checkout attestation v1.
type ResultAttestation struct {
	ResultClaim
	Signature string `json:"signature"`
}

// Validate requires explicit complete bounded evidence, including an explicit empty diff.
func (c ResultClaim) Validate() error {
	if err := c.ResultRequest.Validate(); err != nil {
		return err
	}
	fail := errors.New("result artifact or path evidence is invalid")
	if !fullCommit(c.Head) || c.Paths == nil || len(c.Paths) > 2048 {
		return fail
	}
	total := 0
	for i, value := range c.Paths {
		total += len(value)
		if !utf8.ValidString(value) || value == "" || len(value) > 1024 || total > 64<<10 || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || i > 0 && c.Paths[i-1] >= value {
			return fail
		}
		for _, r := range value {
			if r < 32 || r == 127 {
				return fail
			}
		}
		parts := strings.Split(value, "/")
		if len(parts) > 128 {
			return fail
		}
		for _, part := range parts {
			if part == "" || part == "." || part == ".." {
				return fail
			}
		}
	}
	return nil
}

// ResultKey derives a domain-separated result key from the expected incarnation token.
func ResultKey(token string) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(resultKeyLabel))
	return mac.Sum(nil)
}

func resultMAC(key []byte, c ResultClaim) []byte {
	fields := []string{resultMessageLabel, c.ActorUID, strconv.FormatInt(c.Generation, 10), c.Swarm, c.Step, c.Origin, c.Commit, c.Challenge, strconv.FormatInt(c.Pass, 10), strconv.FormatInt(c.Sequence, 10), c.Head, strconv.Itoa(len(c.Paths))}
	fields = append(fields, c.Paths...)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strings.Join(fields, "\x00")))
	return mac.Sum(nil)
}

// SignResult signs only complete valid result evidence using the result-domain key.
func SignResult(key []byte, c ResultClaim) (string, error) {
	if len(key) == 0 {
		return "", errors.New("result attestation key is required")
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	signature := base64.RawURLEncoding.EncodeToString(resultMAC(key, c))
	wire, err := json.Marshal(ResultAttestation{ResultClaim: c, Signature: signature})
	if err != nil || len(wire) > MaxResultAttestationBytes {
		return "", errors.New("result attestation exceeds size limit")
	}
	return signature, nil
}

// VerifyResult checks observations against the server's exact expected request.
// The key must come from want's identity. Challenge issuance/consumption belongs to the caller.
func VerifyResult(key []byte, want ResultRequest, got ResultAttestation) error {
	if len(key) == 0 {
		return errors.New("result attestation key is required")
	}
	if err := want.Validate(); err != nil {
		return err
	}
	if got.Generation != want.Generation {
		return ErrIncarnation
	}
	if got.ResultRequest != want {
		return ErrMismatch
	}
	if err := got.ResultClaim.Validate(); err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(got.Signature)
	if err != nil || len(signature) != sha256.Size || !hmac.Equal(signature, resultMAC(key, got.ResultClaim)) {
		return ErrSignature
	}
	return nil
}

// DecodeResultAttestation rejects aliases, duplicates, missing/null fields and trailing data.
func DecodeResultAttestation(data []byte) (ResultAttestation, error) {
	fail := errors.New("malformed result attestation")
	if len(data) > MaxResultAttestationBytes || !utf8.Valid(data) || !uniqueFields(data) {
		return ResultAttestation{}, fail
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 12 {
		return ResultAttestation{}, fail
	}
	for _, key := range []string{"actor_uid", "generation", "swarm", "step", "origin", "commit", "challenge", "pass", "sequence", "head", "paths", "signature"} {
		if _, ok := fields[key]; !ok {
			return ResultAttestation{}, fail
		}
	}
	var result ResultAttestation
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return ResultAttestation{}, fail
	}
	if err := result.ResultClaim.Validate(); err != nil {
		return ResultAttestation{}, fail
	}
	return result, nil
}
