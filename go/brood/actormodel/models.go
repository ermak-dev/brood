// Package actormodel validates the Claude Code models an installation assigns
// to its agents (#263): BROOD_ACTOR_MODEL for the main work and
// BROOD_ACTOR_SMALL_MODEL for fast background tasks. It is pure: no
// configuration, Claude Code or network. Errors name the setting and never
// echo the value.
package actormodel

import "errors"

// MaxName bounds a model name in bytes. Provider IDs and inference profile
// ARNs fit well below it, and two names keep the workspace response of the
// guest within its 4 KiB limit.
const MaxName = 256

// RoleModelHeader carries the session-specific override on the guest role route.
const RoleModelHeader = "X-Brood-Role-Model"

// Models are the installation-assigned model names. An empty field leaves the
// choice to Claude Code; the zero value is "no installation models".
type Models struct {
	Model string `json:"model,omitempty"`
	Small string `json:"smallModel,omitempty"`
}

var (
	errModel = errors.New("BROOD_ACTOR_MODEL must be at most 256 printable ASCII characters without spaces")
	errSmall = errors.New("BROOD_ACTOR_SMALL_MODEL must be at most 256 printable ASCII characters without spaces")
)

// IsZero reports the absence of installation models.
func (m Models) IsZero() bool { return m == Models{} }

// Validate checks every set field, whichever side read it.
func (m Models) Validate() error {
	if m.Model != "" && !ValidName(m.Model) {
		return errModel
	}
	if m.Small != "" && !ValidName(m.Small) {
		return errSmall
	}
	return nil
}

// ValidName accepts 1..MaxName bytes of printable ASCII without spaces: the
// name becomes an environment value of Claude Code and a JSON string, so a
// space, control character or non-ASCII byte is a typo, not a model.
func ValidName(name string) bool {
	if name == "" || len(name) > MaxName {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x21 || name[i] > 0x7e {
			return false
		}
	}
	return true
}
