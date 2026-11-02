package projectsecret

import (
	"strings"
	"testing"
)

// BUG: A short logical path may still contain a component the Linux filesystem cannot materialize.
// SPEC: Every component is at most 255 bytes, not runes, before persistence.
func TestSecretPathComponentByteLimit(t *testing.T) {
	for _, name := range []string{strings.Repeat("a", 256), "nested/" + strings.Repeat("é", 128)} {
		if err := ValidateFileName(name); err == nil {
			t.Fatal("unmaterializable file component accepted")
		}
	}
	if err := ValidateFileName("nested/" + strings.Repeat("a", 255)); err != nil {
		t.Fatal(err)
	}
}
