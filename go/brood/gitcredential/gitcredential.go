// Package gitcredential describes server-only Git credentials of a project
// (#173): a name, the exact canonical HTTPS repository the credential is bound
// to and an HTTP Basic username and token. Validation is pure, without I/O.
// Unlike project var/file secrets these values are never delivered to actors:
// only the server's Git gateway puts them into its upstream request.
package gitcredential

import (
	"errors"
	"fmt"
	"io"
	"regexp"

	"brood.dev/go/brood/gitorigin"
)

const (
	// MaxNameBytes bounds a credential name, a lowercase DNS label.
	MaxNameBytes = 63
	// MaxUsernameBytes and MaxTokenBytes match the BROOD_GIT_REMOTES bounds.
	MaxUsernameBytes = 256
	MaxTokenBytes    = 4096
	// MaxPerProject bounds stored credentials of one project.
	MaxPerProject = 16
)

// namePattern — то же правило имени, что у имён Broodfile.
var namePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// Credential is sensitive. Formatting with any verb and JSON hide every
// field, so a credential logged or encoded by mistake discloses nothing; wire
// formats must name the fields explicitly. The only exception is %p of a
// non-pointer: fmt prints it by reflection past every method, and go vet
// rejects that call.
type Credential struct {
	Name       string
	Repository string
	Username   string
	Token      string
}

func (Credential) String() string               { return "gitcredential.Credential{redacted}" }
func (c Credential) GoString() string           { return c.String() }
func (Credential) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Format serves every verb: String and GoString cover only %v, %s, %q, %x,
// %X and %#v, and other verbs would print the fields.
func (c Credential) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// Info is the metadata of one credential. It never carries the username or
// token. Active reports that the project declaration references the
// credential and the project repository equals its bound repository.
type Info struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Active     bool   `json:"active"`
}

// ValidateName accepts a lowercase DNS label of at most MaxNameBytes.
func ValidateName(name string) error {
	if len(name) > MaxNameBytes || !namePattern.MatchString(name) {
		return errors.New("Git credential name must be a lowercase DNS label of at most 63 bytes")
	}
	return nil
}

// ValidateRepository accepts only an already canonical HTTPS repository URL:
// the binding is compared byte for byte.
func ValidateRepository(repository string) error {
	if canonical, err := gitorigin.Canonical(repository); err != nil || canonical != repository {
		return errors.New("Git credential repository must be a canonical HTTPS repository URL without credentials")
	}
	return nil
}

// ValidateUsername accepts visible ASCII without colons: Basic auth splits
// the pair at the first colon.
func ValidateUsername(username string) error {
	if username == "" || len(username) > MaxUsernameBytes || !visibleASCII(username, true) {
		return errors.New("Git credential username must be visible ASCII without colons, at most 256 bytes")
	}
	return nil
}

// ValidateToken accepts visible ASCII of at most MaxTokenBytes.
func ValidateToken(token string) error {
	if token == "" || len(token) > MaxTokenBytes || !visibleASCII(token, false) {
		return errors.New("Git credential token must be visible ASCII, at most 4096 bytes")
	}
	return nil
}

// Validate checks every field of a credential.
func Validate(c Credential) error {
	for _, err := range []error{ValidateName(c.Name), ValidateRepository(c.Repository), ValidateUsername(c.Username), ValidateToken(c.Token)} {
		if err != nil {
			return err
		}
	}
	return nil
}

func visibleASCII(value string, noColon bool) bool {
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x21 || c > 0x7e || noColon && c == ':' {
			return false
		}
	}
	return true
}
