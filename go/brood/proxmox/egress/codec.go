package egress

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// Proxmox limits each bootstrap environment value to 8192 bytes.
const maxPolicyBytes = 8192

var errInvalidPolicy = errors.New("invalid versioned managed egress policy")

// Encode returns the versioned bootstrap representation, without resolver state.
func (p *Policy) Encode() ([]byte, error) {
	if p == nil {
		return nil, errInvalidPolicy
	}
	raw, err := json.Marshal(struct {
		Version int    `json:"version"`
		Rules   []Rule `json:"rules"`
	}{1, p.Rules()})
	if err != nil || len(raw) > maxPolicyBytes {
		return nil, errInvalidPolicy
	}
	return raw, nil
}

// Decode rejects aliases, duplicate keys, unknown fields and oversized documents.
// No invalid input value is included in an error.
func Decode(raw []byte) (*Policy, error) {
	if len(raw) > maxPolicyBytes || !utf8.Valid(raw) {
		return nil, errInvalidPolicy
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	version := 0
	var rules []Rule
	err := readObject(decoder, map[string]func() error{
		"version": func() error { return decoder.Decode(&version) },
		"rules": func() error {
			token, err := decoder.Token()
			if err != nil || token != json.Delim('[') {
				return errInvalidPolicy
			}
			for decoder.More() {
				if len(rules) >= 64 {
					return errInvalidPolicy
				}
				var rule Rule
				if err := readObject(decoder, map[string]func() error{
					"protocol":  func() error { return decoder.Decode(&rule.Protocol) },
					"hostnames": func() error { return decoder.Decode(&rule.Hostnames) },
					"ports":     func() error { return decoder.Decode(&rule.Ports) },
				}); err != nil {
					return err
				}
				rules = append(rules, rule)
			}
			token, err = decoder.Token()
			if err != nil || token != json.Delim(']') {
				return errInvalidPolicy
			}
			return nil
		},
	})
	if err != nil || version != 1 {
		return nil, errInvalidPolicy
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errInvalidPolicy
	}
	policy, err := Compile(rules)
	if err != nil {
		return nil, errInvalidPolicy
	}
	return policy, nil
}

func readObject(decoder *json.Decoder, fields map[string]func() error) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errInvalidPolicy
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errInvalidPolicy
		}
		key, ok := token.(string)
		if !ok || seen[key] || fields[key] == nil {
			return errInvalidPolicy
		}
		seen[key] = true
		if err := fields[key](); err != nil {
			return errInvalidPolicy
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || len(seen) != len(fields) {
		return errInvalidPolicy
	}
	return nil
}
