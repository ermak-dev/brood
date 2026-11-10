// Package gitidentity derives and validates the Git author and committer
// identity of a managed actor (#249). It is pure: no Git, configuration or
// network. Errors describe the violated rule and never echo the value.
package gitidentity

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Bounds of the identity fields in bytes. MaxName leaves room for "brood "
// and a 63-byte DNS label of an actor name; MaxEmail is the RFC 5321 path.
const (
	MaxName  = 128
	MaxEmail = 254
)

// Identity is a Git user.name and user.email pair. The zero value means
// "no identity": the installation has no BROOD_ACTOR_GIT_DOMAIN.
type Identity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

var (
	errActor  = errors.New("actor name must be a nonempty lowercase DNS label")
	errDomain = errors.New("Git identity domain must be a lowercase DNS name")
	errName   = errors.New("Git identity name must be nonempty UTF-8 of at most 128 bytes without control characters, angle brackets or surrounding spaces")
	errEmail  = errors.New("Git identity email must be local@domain of visible ASCII without angle brackets, at most 254 bytes")
)

// Actor derives the identity of actor on the installation domain: name
// "brood <actor>", email "<actor>@<domain>".
func Actor(actor, domain string) (Identity, error) {
	if !dnsLabel(actor) {
		return Identity{}, errActor
	}
	if !ValidDomain(domain) {
		return Identity{}, errDomain
	}
	id := Identity{Name: "brood " + actor, Email: actor + "@" + domain}
	if err := id.Validate(); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// IsZero reports the absence of an identity.
func (i Identity) IsZero() bool { return i == Identity{} }

// Validate checks the form of a received identity, whichever side derived it.
func (i Identity) Validate() error {
	if i.Name == "" || len(i.Name) > MaxName || !utf8.ValidString(i.Name) || strings.TrimSpace(i.Name) != i.Name || strings.ContainsAny(i.Name, "<>") {
		return errName
	}
	for _, r := range i.Name {
		if r < 0x20 || r == 0x7f {
			return errName
		}
	}
	if len(i.Email) > MaxEmail {
		return errEmail
	}
	local, domain, found := strings.Cut(i.Email, "@")
	if !found || local == "" || !ValidDomain(domain) {
		return errEmail
	}
	for i := 0; i < len(local); i++ {
		c := local[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '+' || c == '-') {
			return errEmail
		}
	}
	return nil
}

// ValidDomain accepts a lowercase DNS name of at most 253 bytes: labels of
// letters, digits and inner hyphens, the last one not numeric (an IP form).
func ValidDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if !dnsLabel(label) {
			return false
		}
	}
	return strings.ContainsAny(labels[len(labels)-1], "abcdefghijklmnopqrstuvwxyz")
}

func dnsLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
